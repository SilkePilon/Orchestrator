package bootstrap

import (
	"strings"
	"testing"
)

func newDraftWith(probe *NodeProbe, opts K3sOptions) *BootstrapDraft {
	srv := NewNode(RoleServer)
	srv.Host = "10.0.0.1"
	srv.Label = "srv1"
	d := &BootstrapDraft{
		Options: opts,
		Nodes:   []Node{srv},
		Probes:  map[string]*NodeProbe{srv.ID: probe},
	}
	return d
}

func TestBuildPlan_ServerOnly(t *testing.T) {
	d := newDraftWith(&NodeProbe{
		Distro: "ubuntu", Version: "24.04", Arch: "amd64",
		PkgManager: "apt", Firewall: "ufw",
		HasModBrNetfilter: true, HasModOverlay: true,
	}, K3sOptions{ClusterName: "demo", Channel: "stable"})

	p, err := BuildPlan(d)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.NodeOrder) != 1 {
		t.Fatalf("want 1 node, got %d", len(p.NodeOrder))
	}
	steps := p.NodeSteps[p.NodeOrder[0]]
	if len(steps) == 0 {
		t.Fatal("no steps generated")
	}

	// Modules should be pre-skipped because the probe says they're loaded.
	var modStep *Step
	for i := range steps {
		if strings.HasPrefix(steps[i].ID, "load-modules-") {
			modStep = &steps[i]
		}
	}
	if modStep == nil {
		t.Fatal("missing load-modules step")
	}
	if !modStep.Skip {
		t.Errorf("load-modules should be skipped when both modules already loaded")
	}

	// Server install step uses curl|sh and INSTALL_K3S_CHANNEL.
	var install *Step
	for i := range steps {
		if strings.HasPrefix(steps[i].ID, "install-server-") {
			install = &steps[i]
		}
	}
	if install == nil {
		t.Fatal("missing install-server step")
	}
	if !strings.Contains(install.Command, "INSTALL_K3S_CHANNEL=stable") {
		t.Errorf("install command missing channel: %s", install.Command)
	}
	if !strings.Contains(install.Command, "curl -sfL https://get.k3s.io") {
		t.Errorf("install command missing curl pipe: %s", install.Command)
	}

	// Firewall step should be present and use ufw.
	var fw *Step
	for i := range steps {
		if strings.HasPrefix(steps[i].ID, "open-firewall-") {
			fw = &steps[i]
		}
	}
	if fw == nil {
		t.Fatal("missing firewall step")
	}
	if !strings.Contains(fw.Command, "ufw allow 6443/tcp") {
		t.Errorf("ufw step doesn't open 6443: %s", fw.Command)
	}
}

func TestBuildPlan_ServerAndAgent(t *testing.T) {
	srv := NewNode(RoleServer)
	srv.Host = "10.0.0.1"
	ag := NewNode(RoleAgent)
	ag.Host = "10.0.0.2"
	d := &BootstrapDraft{
		Options: K3sOptions{Channel: "stable"},
		Nodes:   []Node{srv, ag},
		Probes: map[string]*NodeProbe{
			srv.ID: {Arch: "amd64", Firewall: "firewalld"},
			ag.ID:  {Arch: "amd64", Firewall: "firewalld"},
		},
	}

	p, err := BuildPlan(d)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if got := p.NodeOrder; len(got) != 2 || got[0] != srv.ID || got[1] != ag.ID {
		t.Fatalf("node order wrong: %#v", got)
	}

	agentSteps := p.NodeSteps[ag.ID]
	var install *Step
	for i := range agentSteps {
		if strings.HasPrefix(agentSteps[i].ID, "install-agent-") {
			install = &agentSteps[i]
		}
	}
	if install == nil {
		t.Fatal("missing install-agent step")
	}
	if !strings.Contains(install.Command, "K3S_URL=https://10.0.0.1:6443") {
		t.Errorf("agent install missing K3S_URL: %s", install.Command)
	}
	if !strings.Contains(install.Command, TokenPlaceholder) {
		t.Errorf("agent install missing token placeholder: %s", install.Command)
	}

	// Agent firewall opens 10250 but NOT 6443 (api server is server-only).
	var fw *Step
	for i := range agentSteps {
		if strings.HasPrefix(agentSteps[i].ID, "open-firewall-") {
			fw = &agentSteps[i]
		}
	}
	if fw == nil {
		t.Fatal("missing agent firewall step")
	}
	if strings.Contains(fw.Command, "6443") {
		t.Errorf("agent firewall should not open 6443: %s", fw.Command)
	}
	if !strings.Contains(fw.Command, "10250") {
		t.Errorf("agent firewall should open 10250: %s", fw.Command)
	}
}

func TestBuildPlan_SkipInstallWhenSameVersion(t *testing.T) {
	d := newDraftWith(&NodeProbe{
		Arch: "amd64", HasK3s: true, K3sVersion: "v1.31.4+k3s1",
	}, K3sOptions{Version: "v1.31.4+k3s1"})
	p, _ := BuildPlan(d)
	for _, s := range p.NodeSteps[p.NodeOrder[0]] {
		if strings.HasPrefix(s.ID, "install-server-") && !s.Skip {
			t.Errorf("install should be skipped when k3s already at requested version")
		}
	}
}

func TestServerConfigYAML_SANIncludesHost(t *testing.T) {
	n := Node{Host: "k3s.example.com", Label: "srv1"}
	yaml := serverConfigYAML(n, K3sOptions{TLSSANs: []string{"alt.example.com", ""}})
	if !strings.Contains(yaml, "k3s.example.com") {
		t.Errorf("yaml missing server host SAN: %q", yaml)
	}
	if !strings.Contains(yaml, "alt.example.com") {
		t.Errorf("yaml missing user SAN: %q", yaml)
	}
	if !strings.Contains(yaml, "node-name: srv1") {
		t.Errorf("yaml missing node-name: %q", yaml)
	}
}

func TestRewriteKubeconfig(t *testing.T) {
	raw := []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: aGVsbG8=
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    token: abc
`)
	cfg, err := RewriteKubeconfig(raw, "k3s.example.com", "demo")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	c, ok := cfg.Clusters["demo"]
	if !ok {
		t.Fatalf("renamed cluster missing; have keys: %#v", keys(cfg.Clusters))
	}
	if c.Server != "https://k3s.example.com:6443" {
		t.Errorf("server URL not rewritten: %q", c.Server)
	}
	if cfg.CurrentContext != "demo" {
		t.Errorf("current-context not renamed: %q", cfg.CurrentContext)
	}
}

func keys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":              "''",
		"abc":           "abc",
		"a b":           "'a b'",
		"it's":          `'it'\''s'`,
		"v1.31.4+k3s1":  "v1.31.4+k3s1",
		"/usr/local/x":  "/usr/local/x",
		"a;rm -rf /":    "'a;rm -rf /'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
