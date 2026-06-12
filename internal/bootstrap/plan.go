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
	serverProbe := probeOf(d, srv.ID)
	plan.NodeSteps[srv.ID] = serverSteps(*srv, d.Options, serverProbe)

	// The agent install needs the server's hostname so it can build
	// K3S_URL. We don't know the node-token at plan time — the executor
	// will resolve it at runtime via a placeholder.
	for _, a := range d.Agents() {
		plan.NodeSteps[a.ID] = agentSteps(*a, *srv, d.Options, probeOf(d, a.ID), serverJoinHost(*srv, serverProbe))
	}

	return plan, nil
}

// BuildAddNodePlan returns a plan for joining a new agent node to an already-
// running cluster. The server node is only used to read the cluster's
// node-token; no install steps are emitted for it. All listed agent nodes
// receive the full agentSteps sequence.
func BuildAddNodePlan(d *BootstrapDraft) (*Plan, error) {
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
	agents := d.Agents()
	if len(agents) == 0 {
		return nil, fmt.Errorf("no agent node defined")
	}

	plan := &Plan{
		Options:   d.Options,
		NodeSteps: map[string][]Step{},
	}
	plan.NodeOrder = append(plan.NodeOrder, srv.ID)
	for _, a := range agents {
		plan.NodeOrder = append(plan.NodeOrder, a.ID)
	}

	// Server: only read the node-token. The executor captures this step's
	// output and substitutes TokenPlaceholder in the agent install command.
	plan.NodeSteps[srv.ID] = []Step{
		{
			ID:           uid("read-token"),
			Title:        "Read the cluster node-token",
			Description:  "Fetch the join token from the running server. The server itself will not be modified.",
			Command:      "cat /var/lib/rancher/k3s/server/node-token",
			RequiresRoot: true,
			Effect:       EffectReadOnly,
		},
	}

	// New agent(s): full agent install sequence.
	serverProbe := probeOf(d, srv.ID)
	for _, a := range agents {
		plan.NodeSteps[a.ID] = agentSteps(*a, *srv, d.Options, probeOf(d, a.ID), serverJoinHost(*srv, serverProbe))
	}

	return plan, nil
}

// BuildUninstallPlan returns a destructive cleanup plan that removes k3s and
// the files Orchestrator created while bootstrapping. Agents are cleaned before the
// server so the control-plane node is available for as long as possible.
func BuildUninstallPlan(d *BootstrapDraft) (*Plan, error) {
	if d == nil {
		return nil, fmt.Errorf("nil draft")
	}
	if len(d.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes defined")
	}

	plan := &Plan{
		Options:   d.Options,
		NodeSteps: map[string][]Step{},
	}
	for _, n := range d.Nodes {
		if n.Role == RoleAgent {
			plan.NodeOrder = append(plan.NodeOrder, n.ID)
		}
	}
	for _, n := range d.Nodes {
		if n.Role == RoleServer {
			plan.NodeOrder = append(plan.NodeOrder, n.ID)
		}
	}
	for _, nodeID := range plan.NodeOrder {
		for _, n := range d.Nodes {
			if n.ID == nodeID {
				plan.NodeSteps[nodeID] = uninstallSteps(n)
				break
			}
		}
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
	cfg := serverConfigYAML(n, opts, p)
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
		Description:  "Run the official k3s installer with the config above without starting the service yet.",
		Command:      installCmd,
		RequiresRoot: true,
		Effect:       EffectInstall,
		Skip:         skip,
		SkipReason:   skipReason,
	})

	s = append(s, Step{
		ID:           uid("start-server"),
		Title:        "Start k3s server",
		Description:  "Queue the k3s server service to start (non-blocking). An immediate crash is detected within 30 s; full API-server readiness is confirmed by the next step.",
		Command:      startServiceCommand("k3s"),
		RequiresRoot: true,
		Effect:       EffectSystem,
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
		Description:  "The kubeconfig is rewritten client-side to point at the public host and saved into Orchestrator's preferences.",
		Command:      "cat /etc/rancher/k3s/k3s.yaml",
		RequiresRoot: true,
		Effect:       EffectReadOnly,
	})

	return s
}

func agentSteps(n Node, srv Node, opts K3sOptions, p *NodeProbe, joinHost string) []Step {
	var s []Step
	s = append(s, prepSteps(n, opts, p, RoleAgent)...)

	url := fmt.Sprintf("https://%s:6443", joinHost)
	installCmd := installCommand(opts, RoleAgent, url, TokenPlaceholder)
	s = append(s, Step{
		ID:           uid("install-agent"),
		Title:        "Install k3s agent",
		Description:  "Install the k3s agent service without starting it yet. The token is fetched from the server right before this step runs.",
		Command:      installCmd,
		RequiresRoot: true,
		Effect:       EffectInstall,
	})
	s = append(s, Step{
		ID:           uid("start-agent"),
		Title:        "Start k3s agent",
		Description:  "Queue the k3s agent service to start (non-blocking). An immediate crash is detected within 30 s.",
		Command:      startServiceCommand("k3s-agent"),
		RequiresRoot: true,
		Effect:       EffectSystem,
	})
	return s
}

func serverJoinHost(srv Node, probe *NodeProbe) string {
	if probe != nil && probe.NetworkIP != "" {
		return probe.NetworkIP
	}
	return srv.Host
}

// prepSteps are the OS-prep commands common to both server and agent
// installs. They are emitted in a deterministic order regardless of the
// probe so the user always sees the full picture, but pre-skipped when
// the probe shows they are unnecessary.
// updateSystemCommand returns a shell snippet that upgrades all packages on
// the detected distro. When the probe is available the correct package manager
// is known ahead of time; when it is not, the snippet auto-detects at runtime.
// All mainstream Linux distros are covered: apt (Debian/Ubuntu), dnf
// (Fedora/RHEL/Rocky/AlmaLinux), yum (older RHEL/CentOS 7), zypper
// (openSUSE/SLES), pacman (Arch/Manjaro), apk (Alpine).
func updateSystemCommand(p *NodeProbe) string {
	if p != nil && p.PkgManager != "" {
		switch p.PkgManager {
		case "apt":
			return "DEBIAN_FRONTEND=noninteractive apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y -q"
		case "dnf":
			return "dnf upgrade -y --refresh"
		case "yum":
			return "yum update -y"
		case "zypper":
			return "zypper --non-interactive refresh && zypper --non-interactive update"
		case "pacman":
			return "pacman -Syu --noconfirm"
		case "apk":
			return "apk update && apk upgrade"
		}
	}
	// Runtime detection fallback.
	return `if command -v apt-get >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y -q
elif command -v dnf >/dev/null 2>&1; then
  dnf upgrade -y --refresh
elif command -v yum >/dev/null 2>&1; then
  yum update -y
elif command -v zypper >/dev/null 2>&1; then
  zypper --non-interactive refresh && zypper --non-interactive update
elif command -v pacman >/dev/null 2>&1; then
  pacman -Syu --noconfirm
elif command -v apk >/dev/null 2>&1; then
  apk update && apk upgrade
else
  echo 'no recognised package manager found; skipping system update'
fi`
}

func prepSteps(n Node, opts K3sOptions, p *NodeProbe, role NodeRole) []Step {
	var s []Step

	// Optional: update all system packages first.
	updateStep := Step{
		ID:           uid("update-system"),
		Title:        "Update system packages",
		Description:  "Upgrade all installed packages to the latest versions available in the distro repositories. This can take several minutes on a fresh node.",
		Command:      updateSystemCommand(p),
		RequiresRoot: true,
		Effect:       EffectSystem,
	}
	if !opts.UpdateSystem {
		updateStep.Skip = true
		updateStep.SkipReason = "not requested"
	}
	s = append(s, updateStep)

	// Optional: install Docker CE.
	dockerStep := Step{
		ID:           uid("install-docker"),
		Title:        "Install Docker CE",
		Description:  "Install Docker CE via the official get.docker.com convenience script. Works on Debian, Ubuntu, Fedora, RHEL, Rocky, AlmaLinux, and most other mainstream distributions.",
		Command:      "curl -fsSL https://get.docker.com | sh",
		RequiresRoot: true,
		Effect:       EffectInstall,
	}
	if !opts.InstallDocker {
		dockerStep.Skip = true
		dockerStep.SkipReason = "not requested"
	}
	s = append(s, dockerStep)

	// If a previous k3s installation was detected by the probe, run the
	// official uninstall script first so we start from a clean slate.
	// This prevents stale service units, environment files, and CNI state
	// from causing "already activating" or wrong-URL failures on reinstall.
	uninstallCmd := "if [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then /usr/local/bin/k3s-agent-uninstall.sh; elif [ -x /usr/local/bin/k3s-uninstall.sh ]; then /usr/local/bin/k3s-uninstall.sh; else echo 'k3s uninstall script not found; nothing to clean up'; fi"
	if role == RoleServer {
		uninstallCmd = "if [ -x /usr/local/bin/k3s-uninstall.sh ]; then /usr/local/bin/k3s-uninstall.sh; elif [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then /usr/local/bin/k3s-agent-uninstall.sh; else echo 'k3s uninstall script not found; nothing to clean up'; fi"
	}
	uninstallStep := Step{
		ID:           uid("uninstall-existing-k3s"),
		Title:        "Remove existing k3s installation",
		Description:  "Run the official k3s uninstall script to clean up any previous installation before reinstalling. Skipped when no prior install was detected by the probe.",
		Command:      uninstallCmd,
		RequiresRoot: true,
		Effect:       EffectSystem,
	}
	if p == nil || !p.HasK3s {
		uninstallStep.Skip = true
		uninstallStep.SkipReason = "no existing k3s installation detected"
	}
	s = append(s, uninstallStep)

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
		Skip:         p != nil && p.HasModBrNetfilter && p.HasModOverlay,
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
		// Best-effort: we add a permanent inet table named orchestrator-k3s.
		var rules []string
		for _, port := range tcp {
			rules = append(rules, fmt.Sprintf("        tcp dport %s accept", port))
		}
		for _, port := range udp {
			rules = append(rules, fmt.Sprintf("        udp dport %s accept", port))
		}
		nft := "table inet orchestrator_k3s {\n  chain input {\n    type filter hook input priority 0;\n" +
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

func uninstallSteps(n Node) []Step {
	uninstallScript := "if [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then /usr/local/bin/k3s-agent-uninstall.sh; elif [ -x /usr/local/bin/k3s-uninstall.sh ]; then /usr/local/bin/k3s-uninstall.sh; else echo 'k3s uninstall script not found; continuing cleanup'; fi"
	if n.Role == RoleServer {
		uninstallScript = "if [ -x /usr/local/bin/k3s-uninstall.sh ]; then /usr/local/bin/k3s-uninstall.sh; elif [ -x /usr/local/bin/k3s-agent-uninstall.sh ]; then /usr/local/bin/k3s-agent-uninstall.sh; else echo 'k3s uninstall script not found; continuing cleanup'; fi"
	}

	return []Step{
		{
			ID:           uid("uninstall-k3s"),
			Title:        "Run k3s uninstall script",
			Description:  "Run the official k3s uninstall script for this node if it is present.",
			Command:      uninstallScript,
			RequiresRoot: true,
			Effect:       EffectSystem,
		},
		{
			ID:           uid("remove-k3s-files"),
			Title:        "Remove k3s files and directories",
			Description:  "Delete residual k3s, CNI, flannel, kubelet, and Orchestrator-created config paths.",
			Command:      "rm -rf /etc/rancher/k3s /var/lib/rancher/k3s /var/lib/kubelet /var/lib/cni /etc/cni/net.d /opt/cni /run/k3s /run/flannel /var/run/flannel /etc/modules-load.d/k3s.conf /etc/sysctl.d/90-k3s.conf /usr/local/bin/k3s /usr/local/bin/k3s-*",
			RequiresRoot: true,
			Effect:       EffectSystem,
		},
		{
			ID:           uid("restore-fstab"),
			Title:        "Restore fstab backup if present",
			Description:  "Restore /etc/fstab from /etc/fstab.bak when Orchestrator previously disabled swap.",
			Command:      "if [ -f /etc/fstab.bak ]; then cp /etc/fstab.bak /etc/fstab && rm -f /etc/fstab.bak; else echo 'no /etc/fstab.bak found'; fi",
			RequiresRoot: true,
			Effect:       EffectFile,
		},
		{
			ID:           uid("remove-firewall-rules"),
			Title:        "Remove k3s firewall rules",
			Description:  "Best-effort removal of firewall ports and nftables table Orchestrator may have added.",
			Command:      uninstallFirewallCommand(),
			RequiresRoot: true,
			Effect:       EffectFirewall,
		},
	}
}

func uninstallFirewallCommand() string {
	var cmds []string
	for _, port := range []string{"6443/tcp", "10250/tcp", "8472/udp"} {
		cmds = append(cmds, fmt.Sprintf("if command -v firewall-cmd >/dev/null 2>&1; then firewall-cmd --permanent --remove-port=%s || true; fi", port))
		cmds = append(cmds, fmt.Sprintf("if command -v ufw >/dev/null 2>&1; then ufw --force delete allow %s || true; fi", port))
	}
	cmds = append(cmds,
		"if command -v firewall-cmd >/dev/null 2>&1; then firewall-cmd --reload || true; fi",
		"if command -v nft >/dev/null 2>&1; then nft delete table inet orchestrator_k3s || true; fi",
		"if command -v iptables >/dev/null 2>&1; then iptables -D INPUT -p tcp --dport 6443 -j ACCEPT || true; iptables -D INPUT -p tcp --dport 10250 -j ACCEPT || true; iptables -D INPUT -p udp --dport 8472 -j ACCEPT || true; fi",
	)
	return strings.Join(cmds, "\n")
}

// ----- low-level helpers -------------------------------------------------

func installCommand(opts K3sOptions, role NodeRole, k3sURL, token string) string {
	env := []string{"INSTALL_K3S_SKIP_START=true"}
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

func startServiceCommand(unit string) string {
	q := shellQuote(unit)
	// k3s (and k3s-agent) are installed as Type=notify with TimeoutStartSec=0,
	// meaning plain `systemctl start` blocks INDEFINITELY waiting for the
	// service to send sd_notify(READY=1). On first boot this can take well
	// over 5 minutes (containerd init, pause-image pull, embedded-etcd init,
	// CNI setup). Using `--no-block` queues the start and returns immediately,
	// avoiding that hang. We then poll for up to 30 s to catch an immediate
	// crash (failed state). Full readiness is verified by the separate
	// "wait-ready" step that polls `k3s kubectl get nodes`.
	return fmt.Sprintf(
		"systemctl stop %[1]s 2>/dev/null || true; "+
			"systemctl reset-failed %[1]s 2>/dev/null || true; "+
			"systemctl daemon-reload 2>/dev/null || true; "+
			"systemctl start --no-block %[1]s || { echo 'failed to queue %[1]s start'; exit 1; }; "+
			"for i in $(seq 1 6); do "+
			"st=$(systemctl is-active %[1]s 2>/dev/null || echo unknown); "+
			"case \"$st\" in "+
			"active) echo '%[1]s is active'; break;; "+
			"failed) echo %[2]s; systemctl status %[1]s --no-pager || true; journalctl -u %[1]s -n 120 --no-pager || true; exit 1;; "+
			"*) sleep 5;; "+
			"esac; "+
			"done",
		q,
		shellQuote("service failed immediately after start; recent logs follow"),
	)
}

func serverConfigYAML(n Node, opts K3sOptions, p *NodeProbe) string {
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
	// kubeconfig validates, plus the probed server IP used by agents and
	// anything the user added explicitly.
	sans := map[string]struct{}{n.Host: {}}
	if p != nil && p.NetworkIP != "" {
		sans[p.NetworkIP] = struct{}{}
	}
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
	marker := "EOF_ORCHESTRATOR"
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
