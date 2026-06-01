// Package gitops implements detection, installation and lifecycle management
// for GitOps engines (Argo CD and Flux CD) on the connected cluster. It is a
// pure-Go backend with no GTK dependencies; the Adwaita UI lives in
// internal/ui/gitops.
//
// Only one engine may be installed at a time. Installation works by fetching
// the upstream install manifest, splitting it into objects and applying them
// (CRDs and namespaces first). Day-2 operations (sync, refresh, reconcile,
// suspend/resume) are implemented as native Kubernetes patches so no CLI or
// API server port-forward is required.
package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provider identifies a GitOps engine.
type Provider string

const (
	ProviderNone   Provider = ""
	ProviderArgoCD Provider = "argocd"
	ProviderFlux   Provider = "flux"
)

// DisplayName returns a human-friendly engine name.
func (p Provider) DisplayName() string {
	switch p {
	case ProviderArgoCD:
		return "Argo CD"
	case ProviderFlux:
		return "Flux CD"
	default:
		return "None"
	}
}

// Default namespaces for each engine.
const (
	ArgoCDNamespace = "argocd"
	FluxNamespace   = "flux-system"
)

// Well-known GroupVersionResources used across the feature.
var (
	GVRApplication = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	GVRAppProject  = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "appprojects"}
	GVRAppSet      = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applicationsets"}

	GVRKustomization  = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	GVRGitRepository  = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	GVROCIRepository  = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1beta2", Resource: "ocirepositories"}
	GVRHelmRepository = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmrepositories"}
	GVRHelmRelease    = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
)

// GVKs for creating typed unstructured objects.
var (
	gvkApplication   = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}
	gvkAppProject    = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "AppProject"}
	gvkGitRepository = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}
	gvkKustomization = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
)

// Detection reports which engines are present on the cluster.
type Detection struct {
	ArgoCD bool
	Flux   bool
}

// Primary returns the single active provider, preferring Argo CD if (against
// the supported model) both are somehow present.
func (d Detection) Primary() Provider {
	switch {
	case d.ArgoCD:
		return ProviderArgoCD
	case d.Flux:
		return ProviderFlux
	default:
		return ProviderNone
	}
}

// Conflict is true when both engines are installed simultaneously, which is
// unsupported and should be surfaced to the user.
func (d Detection) Conflict() bool { return d.ArgoCD && d.Flux }

// Detect checks for the presence of each engine by looking up its primary CRD.
func Detect(ctx context.Context, c client.Client) (Detection, error) {
	var d Detection
	argo, err := crdExists(ctx, c, "applications.argoproj.io")
	if err != nil {
		return d, err
	}
	d.ArgoCD = argo
	flux, err := crdExists(ctx, c, "kustomizations.kustomize.toolkit.fluxcd.io")
	if err != nil {
		return d, err
	}
	d.Flux = flux
	return d, nil
}

func crdExists(ctx context.Context, c client.Client, name string) (bool, error) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})
	err := c.Get(ctx, client.ObjectKey{Name: name}, crd)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// InstallOptions captures the user's choices on the install screen.
type InstallOptions struct {
	Provider  Provider
	Namespace string
	// Version is a git ref/branch for Argo CD (e.g. "stable", "v2.13.3") or a
	// release tag for Flux (e.g. "latest", "v2.4.0").
	Version string
	// HighAvailability selects the Argo CD HA manifest bundle.
	HighAvailability bool
}

// applyDefaults fills in sensible defaults for empty fields.
func (o *InstallOptions) applyDefaults() {
	switch o.Provider {
	case ProviderArgoCD:
		if o.Namespace == "" {
			o.Namespace = ArgoCDNamespace
		}
		if o.Version == "" {
			o.Version = "stable"
		}
	case ProviderFlux:
		// Flux's manifest is self-contained and hard-codes flux-system, so we
		// pin the namespace regardless of input.
		o.Namespace = FluxNamespace
		if o.Version == "" {
			o.Version = "latest"
		}
	}
}

// manifestURL returns the upstream install manifest URL for the options.
func (o InstallOptions) manifestURL() (string, error) {
	switch o.Provider {
	case ProviderArgoCD:
		path := "manifests/install.yaml"
		if o.HighAvailability {
			path = "manifests/ha/install.yaml"
		}
		return fmt.Sprintf("https://raw.githubusercontent.com/argoproj/argo-cd/%s/%s", o.Version, path), nil
	case ProviderFlux:
		if o.Version == "latest" || o.Version == "" {
			return "https://github.com/fluxcd/flux2/releases/latest/download/install.yaml", nil
		}
		return fmt.Sprintf("https://github.com/fluxcd/flux2/releases/download/%s/install.yaml", o.Version), nil
	default:
		return "", fmt.Errorf("unknown provider %q", o.Provider)
	}
}

// clusterScopedKinds are Kinds that must never receive a namespace.
var clusterScopedKinds = map[string]struct{}{
	"Namespace":                      {},
	"CustomResourceDefinition":       {},
	"ClusterRole":                    {},
	"ClusterRoleBinding":             {},
	"PriorityClass":                  {},
	"APIService":                     {},
	"ValidatingWebhookConfiguration": {},
	"MutatingWebhookConfiguration":   {},
	"StorageClass":                   {},
	"IngressClass":                   {},
	"RuntimeClass":                   {},
	"PodSecurityPolicy":              {},
	"FlowSchema":                     {},
	"PriorityLevelConfiguration":     {},
}

// ProgressFunc receives human-readable progress lines during installation.
type ProgressFunc func(line string)

// Install fetches and applies the engine's install manifest. Progress lines are
// reported via progress (may be nil). The context controls cancellation.
func Install(ctx context.Context, c client.Client, opts InstallOptions, progress ProgressFunc) error {
	opts.applyDefaults()
	log := func(format string, a ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, a...))
		}
	}

	// Guard: refuse to install if the other engine is already present.
	det, err := Detect(ctx, c)
	if err != nil {
		return fmt.Errorf("detecting existing engines: %w", err)
	}
	if opts.Provider == ProviderArgoCD && det.Flux {
		return fmt.Errorf("Flux CD is already installed; uninstall it before installing Argo CD")
	}
	if opts.Provider == ProviderFlux && det.ArgoCD {
		return fmt.Errorf("Argo CD is already installed; uninstall it before installing Flux CD")
	}

	url, err := opts.manifestURL()
	if err != nil {
		return err
	}
	log("Downloading manifest from %s", url)
	raw, err := fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("download manifest: %w", err)
	}
	log("Downloaded %d bytes", len(raw))

	objs, err := SplitYAML(raw)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	log("Parsed %d objects", len(objs))

	// Ensure the target namespace exists (Argo CD's manifest does not include
	// it; Flux's does, but creating it twice is harmless).
	ns := newNamespace(opts.Namespace)
	if err := applyOne(ctx, c, ns); err != nil {
		return fmt.Errorf("create namespace %s: %w", opts.Namespace, err)
	}

	// Argo CD manifests omit the namespace on namespaced objects; inject it.
	if opts.Provider == ProviderArgoCD {
		for _, o := range objs {
			if _, cluster := clusterScopedKinds[o.GetKind()]; !cluster && o.GetNamespace() == "" {
				o.SetNamespace(opts.Namespace)
			}
		}
	}

	// Apply CRDs and namespaces first so dependent objects validate.
	sort.SliceStable(objs, func(i, j int) bool {
		return applyRank(objs[i]) < applyRank(objs[j])
	})

	for i, o := range objs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := applyOne(ctx, c, o); err != nil {
			return fmt.Errorf("apply %s %q: %w", o.GetKind(), o.GetName(), err)
		}
		if (i+1)%25 == 0 || i+1 == len(objs) {
			log("Applied %d/%d objects", i+1, len(objs))
		}
	}
	log("%s installed successfully", opts.Provider.DisplayName())
	return nil
}

func applyRank(o *unstructured.Unstructured) int {
	switch o.GetKind() {
	case "CustomResourceDefinition":
		return 0
	case "Namespace":
		return 1
	default:
		return 2
	}
}

// Uninstall removes the engine. It deletes the install namespace and the
// engine's CRDs (which garbage-collects all custom resources).
func Uninstall(ctx context.Context, c client.Client, provider Provider, progress ProgressFunc) error {
	log := func(format string, a ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, a...))
		}
	}
	var ns string
	var crdSuffixes []string
	switch provider {
	case ProviderArgoCD:
		ns = ArgoCDNamespace
		crdSuffixes = []string{"argoproj.io"}
	case ProviderFlux:
		ns = FluxNamespace
		crdSuffixes = []string{"fluxcd.io"}
	default:
		return fmt.Errorf("unknown provider %q", provider)
	}

	log("Deleting CRDs for %s", provider.DisplayName())
	if err := deleteCRDs(ctx, c, crdSuffixes); err != nil {
		return err
	}

	log("Deleting namespace %s", ns)
	nsObj := newNamespace(ns)
	if err := c.Delete(ctx, nsObj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", ns, err)
	}
	log("%s uninstalled", provider.DisplayName())
	return nil
}

func deleteCRDs(ctx context.Context, c client.Client, suffixes []string) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinitionList",
	})
	if err := c.List(ctx, list); err != nil {
		return fmt.Errorf("list CRDs: %w", err)
	}
	for i := range list.Items {
		crd := &list.Items[i]
		name := crd.GetName()
		match := false
		for _, s := range suffixes {
			if strings.HasSuffix(name, "."+s) || strings.HasSuffix(name, s) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		if err := c.Delete(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete CRD %s: %w", name, err)
		}
	}
	return nil
}

// --- Day-2 operations ----------------------------------------------------

// SyncApplication triggers an Argo CD sync by writing the .operation field,
// which the application-controller reconciles. prune and dryRun tune behaviour.
func SyncApplication(ctx context.Context, c client.Client, namespace, name string, prune, dryRun bool) error {
	patch := fmt.Sprintf(`{"operation":{"initiatedBy":{"username":"orchestrator","automated":false},"sync":{"prune":%t,"dryRun":%t,"syncStrategy":{"hook":{}}}}}`, prune, dryRun)
	return mergePatch(ctx, c, gvkApplication, namespace, name, []byte(patch))
}

// RefreshApplication asks Argo CD to recompute an Application's status. mode is
// "normal" or "hard".
func RefreshApplication(ctx context.Context, c client.Client, namespace, name, mode string) error {
	if mode == "" {
		mode = "normal"
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"argocd.argoproj.io/refresh":%q}}}`, mode)
	return mergePatch(ctx, c, gvkApplication, namespace, name, []byte(patch))
}

// Reconcile triggers a Flux reconciliation for any flux object by stamping the
// requestedAt annotation.
func Reconcile(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string) error {
	now := time.Now().Format(time.RFC3339Nano)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":%q}}}`, now)
	return mergePatch(ctx, c, gvk, namespace, name, []byte(patch))
}

// SetSuspend toggles spec.suspend on a Flux object.
func SetSuspend(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string, suspend bool) error {
	patch := fmt.Sprintf(`{"spec":{"suspend":%t}}`, suspend)
	return mergePatch(ctx, c, gvk, namespace, name, []byte(patch))
}

func mergePatch(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string, patch []byte) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

// --- Object builders -----------------------------------------------------

// ArgoApplicationSpec holds the fields collected by the "New Application" form.
type ArgoApplicationSpec struct {
	Name            string
	Namespace       string // Argo CD namespace (where the Application lives)
	Project         string
	RepoURL         string
	Path            string
	TargetRevision  string
	DestServer      string
	DestNamespace   string
	AutoSync        bool
	SelfHeal        bool
	Prune           bool
	CreateNamespace bool
}

// BuildArgoApplication assembles an unstructured Argo CD Application.
func BuildArgoApplication(s ArgoApplicationSpec) *unstructured.Unstructured {
	if s.Project == "" {
		s.Project = "default"
	}
	if s.TargetRevision == "" {
		s.TargetRevision = "HEAD"
	}
	if s.DestServer == "" {
		s.DestServer = "https://kubernetes.default.svc"
	}
	app := &unstructured.Unstructured{Object: map[string]any{}}
	app.SetGroupVersionKind(gvkApplication)
	app.SetNamespace(s.Namespace)
	app.SetName(s.Name)
	spec := map[string]any{
		"project": s.Project,
		"source": map[string]any{
			"repoURL":        s.RepoURL,
			"path":           s.Path,
			"targetRevision": s.TargetRevision,
		},
		"destination": map[string]any{
			"server":    s.DestServer,
			"namespace": s.DestNamespace,
		},
	}
	if s.AutoSync {
		automated := map[string]any{
			"prune":    s.Prune,
			"selfHeal": s.SelfHeal,
		}
		syncPolicy := map[string]any{"automated": automated}
		if s.CreateNamespace {
			syncPolicy["syncOptions"] = []any{"CreateNamespace=true"}
		}
		spec["syncPolicy"] = syncPolicy
	} else if s.CreateNamespace {
		spec["syncPolicy"] = map[string]any{"syncOptions": []any{"CreateNamespace=true"}}
	}
	app.Object["spec"] = spec
	return app
}

// ArgoProjectSpec holds the fields collected by the "New Project" form.
type ArgoProjectSpec struct {
	Name                       string
	Namespace                  string // Argo CD namespace (where the AppProject lives)
	Description                string
	SourceRepos                []string // allowed source repository URLs ("*" = any)
	DestServer                 string
	DestNS                     string // allowed destination namespace ("*" = any)
	AllowAllClusterResources   bool
	AllowAllNamespaceResources bool
}

// BuildArgoAppProject assembles an unstructured Argo CD AppProject.
func BuildArgoAppProject(s ArgoProjectSpec) *unstructured.Unstructured {
	if s.DestServer == "" {
		s.DestServer = "https://kubernetes.default.svc"
	}
	if s.DestNS == "" {
		s.DestNS = "*"
	}
	repos := s.SourceRepos
	if len(repos) == 0 {
		repos = []string{"*"}
	}
	sourceRepos := make([]any, 0, len(repos))
	for _, r := range repos {
		sourceRepos = append(sourceRepos, r)
	}

	proj := &unstructured.Unstructured{Object: map[string]any{}}
	proj.SetGroupVersionKind(gvkAppProject)
	proj.SetNamespace(s.Namespace)
	proj.SetName(s.Name)
	spec := map[string]any{
		"sourceRepos": sourceRepos,
		"destinations": []any{
			map[string]any{
				"server":    s.DestServer,
				"namespace": s.DestNS,
			},
		},
	}
	if s.Description != "" {
		spec["description"] = s.Description
	}
	if s.AllowAllClusterResources {
		spec["clusterResourceWhitelist"] = []any{
			map[string]any{"group": "*", "kind": "*"},
		}
	}
	if s.AllowAllNamespaceResources {
		spec["namespaceResourceWhitelist"] = []any{
			map[string]any{"group": "*", "kind": "*"},
		}
	}
	proj.Object["spec"] = spec
	return proj
}

// form, including an inline GitRepository source.
type FluxKustomizationSpec struct {
	Name         string
	Namespace    string
	RepoURL      string
	Branch       string
	Path         string
	Interval     string
	Prune        bool
	TargetNS     string
	SourceName   string // GitRepository name (defaults to Name)
	CreateSource bool
}

// BuildFluxObjects assembles a GitRepository (optional) and a Kustomization.
func BuildFluxObjects(s FluxKustomizationSpec) []*unstructured.Unstructured {
	if s.Namespace == "" {
		s.Namespace = FluxNamespace
	}
	if s.Interval == "" {
		s.Interval = "5m"
	}
	if s.Branch == "" {
		s.Branch = "main"
	}
	if s.Path == "" {
		s.Path = "./"
	}
	if s.SourceName == "" {
		s.SourceName = s.Name
	}
	var out []*unstructured.Unstructured
	if s.CreateSource {
		git := &unstructured.Unstructured{Object: map[string]any{}}
		git.SetGroupVersionKind(gvkGitRepository)
		git.SetNamespace(s.Namespace)
		git.SetName(s.SourceName)
		git.Object["spec"] = map[string]any{
			"interval": s.Interval,
			"url":      s.RepoURL,
			"ref":      map[string]any{"branch": s.Branch},
		}
		out = append(out, git)
	}

	kust := &unstructured.Unstructured{Object: map[string]any{}}
	kust.SetGroupVersionKind(gvkKustomization)
	kust.SetNamespace(s.Namespace)
	kust.SetName(s.Name)
	spec := map[string]any{
		"interval": s.Interval,
		"path":     s.Path,
		"prune":    s.Prune,
		"sourceRef": map[string]any{
			"kind": "GitRepository",
			"name": s.SourceName,
		},
	}
	if s.TargetNS != "" {
		spec["targetNamespace"] = s.TargetNS
	}
	kust.Object["spec"] = spec
	out = append(out, kust)
	return out
}

// Create applies a single object using create-or-update semantics.
func Create(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	return applyOne(ctx, c, obj)
}

// --- helpers -------------------------------------------------------------

func newNamespace(name string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{Object: map[string]any{}}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	ns.SetName(name)
	return ns
}

func applyOne(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

// SplitYAML decodes a multi-document YAML stream into unstructured objects,
// skipping empty documents and List wrappers.
func SplitYAML(raw []byte) ([]*unstructured.Unstructured, error) {
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.GetKind() == "" {
			continue
		}
		// Flatten *List objects into their items.
		if strings.HasSuffix(u.GetKind(), "List") {
			if items, found, _ := unstructured.NestedSlice(u.Object, "items"); found {
				for _, it := range items {
					if im, ok := it.(map[string]any); ok {
						out = append(out, &unstructured.Unstructured{Object: im})
					}
				}
				continue
			}
		}
		out = append(out, u)
	}
	return out, nil
}

// ListVersions fetches the available release versions for the given provider
// from the GitHub Releases API. The returned slice always begins with a
// "default" sentinel ("stable" for Argo CD, "latest" for Flux) followed by the
// most recent stable release tags (newest first). Pre-releases and drafts are
// omitted. Errors are returned so the caller can fall back to a free-form value.
func ListVersions(ctx context.Context, provider Provider) ([]string, error) {
	var repo, sentinel string
	switch provider {
	case ProviderArgoCD:
		repo, sentinel = "argoproj/argo-cd", "stable"
	case ProviderFlux:
		repo, sentinel = "fluxcd/flux2", "latest"
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d fetching releases for %s", resp.StatusCode, repo)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var releases []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, err
	}

	versions := []string{sentinel}
	for _, r := range releases {
		if r.Prerelease || r.Draft || r.TagName == "" {
			continue
		}
		versions = append(versions, r.TagName)
	}
	return versions, nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// keep metav1 import (used by callers via re-export expectations)
var _ = metav1.ObjectMeta{}
