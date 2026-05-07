package bootstrap

import (
	"fmt"
	"net"
	"strconv"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// RewriteKubeconfig parses raw k3s.yaml output, rewrites every
// cluster's server URL so its host points at publicHost:6443 (the
// host the user typed for the server, not 127.0.0.1 as k3s writes by
// default), and renames the "default" context, cluster, and user to
// clusterName so multiple bootstrapped clusters can coexist in one
// kubeconfig file.
func RewriteKubeconfig(raw []byte, publicHost, clusterName string) (*clientcmdapi.Config, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if clusterName == "" {
		clusterName = "k3s"
	}

	// Rewrite server hosts on every cluster entry.
	for _, c := range cfg.Clusters {
		if c == nil || c.Server == "" {
			continue
		}
		newURL, err := replaceHost(c.Server, publicHost)
		if err != nil {
			return nil, err
		}
		c.Server = newURL
	}

	// Rename default -> clusterName for cluster, user, and context, so
	// the file is self-describing when merged with the user's other
	// kubeconfigs.
	renameKey(cfg.Clusters, "default", clusterName)
	renameKey(cfg.AuthInfos, "default", clusterName)
	renameContexts(cfg, "default", clusterName)
	cfg.CurrentContext = clusterName
	return cfg, nil
}

func replaceHost(serverURL, host string) (string, error) {
	// k3s writes "https://127.0.0.1:6443"; we only need to swap the host
	// part. Keep the port so users can override the default when they
	// front k3s with a load balancer.
	const scheme = "https://"
	if len(serverURL) < len(scheme) {
		return "", fmt.Errorf("kubeconfig server URL too short: %q", serverURL)
	}
	rest := serverURL[len(scheme):]
	// rest is host:port[/path]
	port := "6443"
	if h, p, err := net.SplitHostPort(rest); err == nil {
		port = p
		_ = h
	}
	if _, err := strconv.Atoi(port); err != nil {
		port = "6443"
	}
	return scheme + net.JoinHostPort(host, port), nil
}

func renameKey[T any](m map[string]*T, from, to string) {
	if m == nil {
		return
	}
	v, ok := m[from]
	if !ok {
		return
	}
	delete(m, from)
	m[to] = v
}

func renameContexts(cfg *clientcmdapi.Config, from, to string) {
	c, ok := cfg.Contexts[from]
	if !ok {
		return
	}
	if c.Cluster == from {
		c.Cluster = to
	}
	if c.AuthInfo == from {
		c.AuthInfo = to
	}
	delete(cfg.Contexts, from)
	cfg.Contexts[to] = c
}
