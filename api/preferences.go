package api

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/internal/pubsub"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

type basePreferences struct {
	ColorScheme adw.ColorScheme
	Clusters    []ClusterPreferences
	License     *License
}

type License struct {
	ID        string
	Key       string
	ExpiresAt *time.Time
}

type Preferences struct {
	*basePreferences
	Clusters []pubsub.Property[ClusterPreferences]
}

func prefsPath() string {
	cd, err := os.UserConfigDir()
	if err != nil {
		panic(err)
	}
	return path.Join(cd, "orchestrator", "prefs.json")
}

type ClusterPreferences struct {
	Kubeconfig          *Kubeconfig
	Name                string
	Host                string
	BearerToken         string
	TLS                 rest.TLSClientConfig
	Exec                *api.ExecConfig
	ReadOnly            bool
	SkipTlsVerification bool
	Navigation          struct {
		Favourites []schema.GroupVersionResource
		Pins       []corev1.ObjectReference
	}
	// Bootstrap, when non-nil, indicates this cluster was created by the
	// Orchestrator k3s bootstrap wizard. It is purely informational; the
	// connection details live in Host/TLS/BearerToken.
	Bootstrap *BootstrapRecord `json:",omitempty"`
}

// BootstrapRecord is metadata about a cluster Orchestrator bootstrapped itself.
// It is rendered on the cluster card so the user can see at a glance how
// the cluster was created and which nodes it spans.
type BootstrapRecord struct {
	Distribution string   // always "k3s" in v1
	Version      string   // resolved k3s version
	Channel      string   // user-selected channel (stable/latest/...)
	ServerHost   string   // public host of the server node
	AgentHosts   []string // public hosts of any agent nodes
	Nodes        []BootstrapNodeRecord
	CreatedAt    time.Time
}

// BootstrapNodeRecord stores non-secret SSH metadata for nodes Orchestrator
// bootstrapped. Secrets such as SSH passwords, sudo passwords, private-key
// contents, and tokens are intentionally not persisted.
type BootstrapNodeRecord struct {
	Role           string
	Host           string
	Port           int
	User           string
	Auth           string
	PrivateKeyPath string
	Become         string
	Label          string
}

// ClusterPreferencesFromKubeconfig builds a self-contained
// ClusterPreferences (no Kubeconfig pointer; everything embedded) from
// a parsed kubeconfig produced by bootstrap.RewriteKubeconfig. The
// resulting cluster survives even if the user never writes the
// kubeconfig out to disk.
func ClusterPreferencesFromKubeconfig(name string, cfg *api.Config, rec *BootstrapRecord) (ClusterPreferences, error) {
	if cfg == nil {
		return ClusterPreferences{}, errors.New("nil kubeconfig")
	}
	ctxName := cfg.CurrentContext
	ctx, ok := cfg.Contexts[ctxName]
	if !ok {
		return ClusterPreferences{}, errors.New("kubeconfig has no current-context")
	}
	cluster, ok := cfg.Clusters[ctx.Cluster]
	if !ok {
		return ClusterPreferences{}, errors.New("kubeconfig current-context references missing cluster")
	}
	user, ok := cfg.AuthInfos[ctx.AuthInfo]
	if !ok {
		return ClusterPreferences{}, errors.New("kubeconfig current-context references missing user")
	}

	prefs := ClusterPreferences{
		Name:      name,
		Host:      cluster.Server,
		Bootstrap: rec,
	}
	prefs.TLS.CAData = cluster.CertificateAuthorityData
	prefs.TLS.CertData = user.ClientCertificateData
	prefs.TLS.KeyData = user.ClientKeyData
	prefs.BearerToken = user.Token
	prefs.SkipTlsVerification = cluster.InsecureSkipTLSVerify
	prefs.Defaults()
	return prefs, nil
}

type Kubeconfig struct {
	Path    string
	Context string
}

func LoadPreferences() (*Preferences, error) {

	var base basePreferences
	if _, err := os.Stat(prefsPath()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else {
		f, err := os.Open(prefsPath())
		if err != nil {
			return nil, err
		}
		if err := json.NewDecoder(f).Decode(&base); err != nil {
			return nil, err
		}
	}
	base.Defaults()

	for i := len(base.Clusters) - 1; i >= 0; i-- {
		config := base.Clusters[i].Kubeconfig
		if config == nil {
			continue
		}
		var prefs = base.Clusters[i]
		if err := UpdateClusterPreferences(&prefs, config.Path, config.Context); err != nil {
			base.Clusters = append(base.Clusters[:i], base.Clusters[i+1:]...)
		} else {
			base.Clusters[i] = prefs
		}
	}

	home, _ := os.UserHomeDir()
	for _, path := range append([]string{path.Join(home, ".kube/config")}, strings.Split(os.Getenv("KUBECONFIG"), ":")...) {
		if _, err := os.Stat(path); err != nil {
			continue
		}

		config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: path}, nil).
			ConfigAccess().GetStartingConfig()
		if err != nil {
			continue
		}

	context:
		for context := range config.Contexts {
			for _, c := range base.Clusters {
				if c.Kubeconfig != nil && c.Kubeconfig.Path == path && c.Kubeconfig.Context == context {
					continue context
				}
			}
			prefs := ClusterPreferences{Kubeconfig: &Kubeconfig{Path: path, Context: context}}
			prefs.Defaults()
			if err := UpdateClusterPreferences(&prefs, path, context); err == nil {
				base.Clusters = append(base.Clusters, prefs)
			}
		}

	}

	prefs := Preferences{
		basePreferences: &base,
	}

	for _, cluster := range base.Clusters {
		prefs.Clusters = append(prefs.Clusters, pubsub.NewProperty(cluster))
	}

	return &prefs, nil
}

func (c *basePreferences) Defaults() {
	for i := range c.Clusters {
		c.Clusters[i].Defaults()
	}
}

func (c *ClusterPreferences) Defaults() {
	if len(c.Navigation.Favourites) == 0 {
		c.Navigation.Favourites = []schema.GroupVersionResource{
			{
				Group:    corev1.SchemeGroupVersion.Group,
				Version:  corev1.SchemeGroupVersion.Version,
				Resource: "pods",
			},
			{
				Group:    corev1.SchemeGroupVersion.Group,
				Version:  corev1.SchemeGroupVersion.Version,
				Resource: "configmaps",
			},
			{
				Group:    corev1.SchemeGroupVersion.Group,
				Version:  corev1.SchemeGroupVersion.Version,
				Resource: "secrets",
			},
			{
				Group:    corev1.SchemeGroupVersion.Group,
				Version:  corev1.SchemeGroupVersion.Version,
				Resource: "persistentvolumeclaims",
			},
			{
				Group:    appsv1.SchemeGroupVersion.Group,
				Version:  appsv1.SchemeGroupVersion.Version,
				Resource: "deployments",
			},
			{
				Group:    appsv1.SchemeGroupVersion.Group,
				Version:  appsv1.SchemeGroupVersion.Version,
				Resource: "statefulsets",
			},
			{
				Group:    corev1.SchemeGroupVersion.Group,
				Version:  corev1.SchemeGroupVersion.Version,
				Resource: "services",
			},
			{
				Group:    networkingv1.SchemeGroupVersion.Group,
				Version:  networkingv1.SchemeGroupVersion.Version,
				Resource: "ingresses",
			},
			{
				Group:    corev1.SchemeGroupVersion.Group,
				Version:  corev1.SchemeGroupVersion.Version,
				Resource: "namespaces",
			},
			{
				Group:    corev1.SchemeGroupVersion.Group,
				Version:  corev1.SchemeGroupVersion.Version,
				Resource: "nodes",
			},
		}
	}
}

func (c *Preferences) Save() error {
	if err := os.MkdirAll(path.Dir(prefsPath()), os.ModePerm); err != nil {
		return err
	}
	f, err := os.Create(prefsPath())
	if err != nil {
		return err
	}

	c.basePreferences.Clusters = []ClusterPreferences{}
	for _, v := range c.Clusters {
		c.basePreferences.Clusters = append(c.basePreferences.Clusters, v.Value())
	}

	if err := json.NewEncoder(f).Encode(c.basePreferences); err != nil {
		return err
	}

	return nil
}

func UpdateClusterPreferences(prefs *ClusterPreferences, path, context string) error {
	var overrides *clientcmd.ConfigOverrides
	if context != "" {
		overrides = &clientcmd.ConfigOverrides{CurrentContext: context}
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: path}, overrides)
	config, err := cc.ClientConfig()
	if err != nil {
		return err
	}

	prefs.Host = config.Host
	prefs.Exec = config.ExecProvider
	prefs.TLS = config.TLSClientConfig
	prefs.SkipTlsVerification = config.TLSClientConfig.Insecure

	if prefs.Name == "" {
		prefs.Name = context
		if prefs.Name == "" {
			c, err := cc.ConfigAccess().GetStartingConfig()
			if err == nil {
				prefs.Name = c.CurrentContext
			}
		}
	}

	if config.CertFile != "" {
		data, err := os.ReadFile(config.CertFile)
		if err != nil {
			return err
		}
		prefs.TLS.CertData = data
		prefs.TLS.CertFile = ""
	}
	if config.KeyFile != "" {
		data, err := os.ReadFile(config.KeyFile)
		if err != nil {
			return err
		}
		prefs.TLS.KeyData = data
		prefs.TLS.KeyFile = ""
	}
	if config.CAFile != "" {
		data, err := os.ReadFile(config.CAFile)
		if err != nil {
			return err
		}
		prefs.TLS.CAData = data
		prefs.TLS.CAFile = ""
	}
	if config.BearerTokenFile != "" {
		data, err := os.ReadFile(config.BearerTokenFile)
		if err != nil {
			return err
		}
		prefs.BearerToken = string(data)
	} else {
		prefs.BearerToken = config.BearerToken
	}

	return nil
}
