package bootstrap

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// BuildPlan returns the per-node ordered list of Steps that, when run by
// the executor, install a fresh k3s cluster matching opts. Probes is the
// map populated by the Probe page (Node.ID -> *NodeProbe).
//
// This is the SINGLE source of truth for every shell command the
// bootstrapper will ever run. Anything that ends up on a remote host
// must originate here so the user can inspect (and edit) it on the Plan
// page before approving.
func BuildPlan(d *BootstrapDraft) (*Plan, error) {
	if d == nil {
		return nil, fmt.Errorf("nil draft")
	}
	srv := d.Server()
	if srv == nil {
		return nil, fmt.Errorf("no server node defined")
	}
	if srv.Host == "" {
		return nil, fmt.Errorf("server node has no host")
	}

	plan := &Plan{
		Options:   d.Options,
		NodeSteps: map[string][]Step{},
	}
	plan.NodeOrder = append(plan.NodeOrder, srv.ID)
	for _, a := range d.Agents() {
		plan.NodeOrder = append(plan.NodeOrder, a.ID)
	}

	// Server steps.
	plan.NodeSteps[srv.ID] = serverSteps(*srv, d.Options, probeOf(d, srv.ID))

	// The agent install needs the server's hostname so it can build
	// K3S_URL. We don't know the node-token at plan time — the executor
	// will resolve it at runtime via a placeholder.
	for _, a := range d.Agents() {
		plan.NodeSteps[a.ID] = agentSteps(*a, *srv, d.Options, probeOf(d, a.ID))
	}

	return plan, nil
}

func probeOf(d *BootstrapDraft, id string) *NodeProbe {
	if d.Probes == nil {
		return nil
	}
	return d.Probes[id]
}

// TokenPlaceholder is substituted by the executor with the node-token
// fetched from the server right before agents are installed.
const TokenPlaceholder = "__K3S_NODE_TOKEN__"

// ----- step builders -----------------------------------------------------

func serverSteps(n Node, opts K3sOptions, p *NodeProbe) []Step {
	var s []Step

	s = append(s, prepSteps(n, opts, p, RoleServer)...)

	// Write /etc/rancher/k3s/config.yaml from the user's options. We use
	// a heredoc so the user sees the exact file contents in the Plan
	// page.
	cfg := serverConfigYAML(n, opts)
	s = append(s, Step{
		ID:           uid("write-config"),
		Title:        "Write /etc/rancher/k3s/config.yaml",
		Description:  "Persist k3s server configuration to disk before installing.",
		Command:      heredocWrite("/etc/rancher/k3s/config.yaml", cfg),
		RequiresRoot: true,
		Effect:       EffectFile,
	})

	// Install k3s.
	installCmd := installCommand(opts, RoleServer, "", "")
	skip := false
	skipReason := ""
	if p != nil && p.HasK3s && opts.Version != "" && p.K3sVersion == opts.Version {
		skip = true
		skipReason = fmt.Sprintf("k3s already at %s", opts.Version)
	}
	s = append(s, Step{
		ID:           uid("install-server"),
		Title:        "Install k3s server",
		Description:  "Run the official k3s installer with the config above.",
		Command:      installCmd,
		RequiresRoot: true,
		Effect:       EffectInstall,
		Skip:         skip,
		SkipReason:   skipReason,
	})

	// Wait for the API server to become ready.
	s = append(s, Step{
		ID:           uid("wait-ready"),
		Title:        "Wait for the node to become Ready",
		Description:  "Poll k3s kubectl until the server reports Ready (up to 5 min).",
		Command:      `for i in $(seq 1 60); do k3s kubectl get nodes 2>/dev/null | awk 'NR==2 {print $2; exit}' | grep -q '^Ready$' && exit 0; sleep 5; done; echo "node not Ready after 5 min"; k3s kubectl get nodes; exit 1`,
		RequiresRoot: true,
		Effect:       EffectReadOnly,
	})

	// Read the node-token. The output of this command is captured by
	// the executor and substituted into TokenPlaceholder for agents.
	s = append(s, Step{
		ID:           uid("read-token"),
		Title:        "Read the cluster node-token",
		Description:  "The token is needed by agent nodes to join the cluster.",
		Command:      "cat /var/lib/rancher/k3s/server/node-token",
		RequiresRoot: true,
		Effect:       EffectReadOnly,
	})

	// Read the kubeconfig. The executor captures stdout and rewrites
	// 'server:' to point at the public host.
	s = append(s, Step{
		ID:           uid("read-kubeconfig"),
		Title:        "Read /etc/rancher/k3s/k3s.yaml",
		Description:  "The kubeconfig is rewritten client-side to point at the public host and saved into Seabird's preferences.",
		Command:      "cat /etc/rancher/k3s/k3s.yaml",
		RequiresRoot: true,
		Effect:       EffectReadOnly,
	})

	return s
}

func agentSteps(n Node, srv Node, opts K3sOptions, p *NodeProbe) []Step {
	var s []Step
	s = append(s, prepSteps(n, opts, p, RoleAgent)...)

	url := fmt.Sprintf("https://%s:6443", srv.Host)
	installCmd := installCommand(opts, RoleAgent, url, TokenPlaceholder)
	s = append(s, Step{
		ID:           uid("install-agent"),
		Title:        "Install k3s agent",
		Description:  "Join this node to the cluster as an agent. The token is fetched from the server right before this step runs.",
		Command:      installCmd,
		RequiresRoot: true,
		Effect:       EffectInstall,
	})
	return s
}

// prepSteps are the OS-prep commands common to both server and agent
// installs. They are emitted in a deterministic order regardless of the
// probe so the user always sees the full picture, but pre-skipped when
// the probe shows they are unnecessary.
func prepSteps(n Node, opts K3sOptions, p *NodeProbe, role NodeRole) []Step {
	var s []Step

	s = append(s, Step{
		ID:           uid("mkdir-rancher"),
		Title:        "Create /etc/rancher/k3s",
		Description:  "Holds the k3s server config (idempotent).",
		Command:      "mkdir -p /etc/rancher/k3s",
		RequiresRoot: true,
		Effect:       EffectIdempotent,
	})

	swapCmd := "swapoff -a && sed -i.bak '/\\sswap\\s/s/^/#/' /etc/fstab"
	swapStep := Step{
		ID:           uid("swap-off"),
		Title:        "Disable swap",
		Description:  "Kubernetes requires swap to be off. The original /etc/fstab is backed up to /etc/fstab.bak.",
		Command:      swapCmd,
		RequiresRoot: true,
		Effect:       EffectSystem,
	}
	if p != nil && !p.SwapEnabled {
		swapStep.Skip = true
		swapStep.SkipReason = "swap is already off"
	}
	s = append(s, swapStep)

	if fwSteps := firewallSteps(p, role); len(fwSteps) > 0 {
		s = append(s, fwSteps...)
	}

	s = append(s, Step{
		ID:           uid("load-modules"),
		Title:        "Load br_netfilter and overlay kernel modules",
		Description:  "Ensure required modules are loaded now and on every boot.",
		Command:      "modprobe br_netfilter && modprobe overlay && printf 'br_netfilter\\noverlay\\n' > /etc/modules-load.d/k3s.conf",
		RequiresRoot: true,
		Effect:       EffectSystem,
		Skip: p != nil && p.HasModBrNetfilter && p.HasModOverlay,
		SkipReason: func() string {
			if p != nil && p.HasModBrNetfilter && p.HasModOverlay {
				return "both modules already loaded"
			}
			return ""
		}(),
	})

	return s
}

func firewallSteps(p *NodeProbe, role NodeRole) []Step {
	if p == nil || p.Firewall == "" {
		return nil
	}
	// Ports k3s needs:
	//   server: 6443/tcp (api), 8472/udp (flannel-vxlan), 10250/tcp (kubelet)
	//   agent:  same minus 6443/tcp inbound (still needs outbound to server)
	tcp := []string{"6443", "10250"}
	udp := []string{"8472"}
	if role == RoleAgent {
		tcp = []string{"10250"}
	}

	var cmds []string
	switch p.Firewall {
	case "firewalld":
		for _, port := range tcp {
			cmds = append(cmds, fmt.Sprintf("firewall-cmd --permanent --add-port=%s/tcp", port))
		}
		for _, port := range udp {
			cmds = append(cmds, fmt.Sprintf("firewall-cmd --permanent --add-port=%s/udp", port))
		}
		cmds = append(cmds, "firewall-cmd --reload")
	case "ufw":
		for _, port := range tcp {
			cmds = append(cmds, fmt.Sprintf("ufw allow %s/tcp", port))
		}
		for _, port := range udp {
			cmds = append(cmds, fmt.Sprintf("ufw allow %s/udp", port))
		}
	case "nftables":
		// Best-effort: we add a permanent inet table named seabird-k3s.
		var rules []string
		for _, port := range tcp {
			rules = append(rules, fmt.Sprintf("        tcp dport %s accept", port))
		}
		for _, port := range udp {
			rules = append(rules, fmt.Sprintf("        udp dport %s accept", port))
		}
		nft := "table inet seabird_k3s {\n  chain input {\n    type filter hook input priority 0;\n" +
			strings.Join(rules, "\n") + "\n  }\n}\n"
		cmds = append(cmds, fmt.Sprintf("nft -f - <<'EOF'\n%sEOF", nft))
	case "iptables":
		for _, port := range tcp {
			cmds = append(cmds, fmt.Sprintf("iptables -I INPUT -p tcp --dport %s -j ACCEPT", port))
		}
		for _, port := range udp {
			cmds = append(cmds, fmt.Sprintf("iptables -I INPUT -p udp --dport %s -j ACCEPT", port))
		}
	default:
		return nil
	}

	return []Step{{
		ID:           uid("open-firewall"),
		Title:        fmt.Sprintf("Open firewall ports (%s)", p.Firewall),
		Description:  "Open the ports k3s needs through the detected firewall.",
		Command:      strings.Join(cmds, " && "),
		RequiresRoot: true,
		Effect:       EffectFirewall,
	}}
}

// ----- low-level helpers -------------------------------------------------

func installCommand(opts K3sOptions, role NodeRole, k3sURL, token string) string {
	var env []string
	if opts.Channel != "" {
		env = append(env, "INSTALL_K3S_CHANNEL="+shellQuote(opts.Channel))
	}
	if opts.Version != "" {
		env = append(env, "INSTALL_K3S_VERSION="+shellQuote(opts.Version))
	}
	if opts.LocalBinaryPath != "" {
		env = append(env, "INSTALL_K3S_SKIP_DOWNLOAD=true",
			"INSTALL_K3S_BIN_DIR_READ_ONLY=true")
	}
	if role == RoleAgent {
		env = append(env, "K3S_URL="+shellQuote(k3sURL), "K3S_TOKEN="+shellQuote(token))
	}
	envStr := strings.Join(env, " ")
	roleArg := "server --config /etc/rancher/k3s/config.yaml"
	if role == RoleAgent {
		roleArg = "agent"
	}

	if opts.LocalBinaryPath != "" {
		// User supplied a binary; drop it in /usr/local/bin first then run
		// the installer with download skipped. The binary must already be
		// present on the remote host at LocalBinaryPath.
		return fmt.Sprintf("install -m 755 %s /usr/local/bin/k3s && curl -sfL https://get.k3s.io | %s sh -s - %s",
			shellQuote(opts.LocalBinaryPath), envStr, roleArg)
	}
	return fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -s - %s", envStr, roleArg)
}

func serverConfigYAML(n Node, opts K3sOptions) string {
	var b strings.Builder
	if opts.ClusterName != "" {
		fmt.Fprintf(&b, "# cluster: %s\n", opts.ClusterName)
	}
	// Always bind to all interfaces so the rewritten kubeconfig works
	// from the user's laptop.
	b.WriteString("write-kubeconfig-mode: \"0644\"\n")

	if opts.CNI == "none" {
		b.WriteString("flannel-backend: none\n")
		b.WriteString("disable-network-policy: true\n")
	}

	disabled := append([]string{}, opts.DisableComponents...)
	sort.Strings(disabled)
	for _, c := range disabled {
		fmt.Fprintf(&b, "disable: %s\n", shellQuote(c))
	}

	if opts.ClusterCIDR != "" {
		fmt.Fprintf(&b, "cluster-cidr: %s\n", shellQuote(opts.ClusterCIDR))
	}
	if opts.ServiceCIDR != "" {
		fmt.Fprintf(&b, "service-cidr: %s\n", shellQuote(opts.ServiceCIDR))
	}

	// TLS SANs always include the server's host so the rewritten
	// kubeconfig validates, plus anything the user added explicitly.
	sans := map[string]struct{}{n.Host: {}}
	for _, s := range opts.TLSSANs {
		if s != "" {
			sans[s] = struct{}{}
		}
	}
	// strip duplicate (e.g. user typed the same host) and sort for
	// determinism.
	list := make([]string, 0, len(sans))
	for s := range sans {
		// Don't add empty-string SANs.
		if s == "" {
			continue
		}
		list = append(list, s)
	}
	sort.Strings(list)
	if len(list) > 0 {
		b.WriteString("tls-san:\n")
		for _, s := range list {
			fmt.Fprintf(&b, "  - %s\n", shellQuote(s))
		}
	}

	if n.Label != "" {
		fmt.Fprintf(&b, "node-name: %s\n", shellQuote(n.Label))
	}
	return b.String()
}

func heredocWrite(path, content string) string {
	// Use a unique marker so embedded EOFs in the content don't break
	// the heredoc. The literal content is not expanded by the shell
	// because we quote the marker.
	marker := "EOF_SEABIRD"
	return fmt.Sprintf("cat > %s <<'%s'\n%s%s\n", path, marker, content, marker)
}

// shellQuote returns s as a POSIX-shell single-quoted token, safe to
// embed in a command string.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !needsQuoting(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsQuoting(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '+' || r == ',':
		default:
			return true
		}
	}
	return false
}

// uid returns a short, stable-ish id with a prefix to make events readable
// in logs. The full UUID is appended for uniqueness within a Plan.
func uid(prefix string) string {
	u := uuid.NewString()
	return prefix + "-" + u[:8]
}

// EnsureHostResolves is a small precondition check used by the Nodes
// page; it has no plan relevance but lives here so all networking-
// related logic stays adjacent.
func EnsureHostResolves(host string) error {
	if _, err := net.LookupHost(host); err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	return nil
}
