// Package bootstrap implements a transparent k3s cluster bootstrapper that
// runs over SSH against user-supplied nodes. It is intentionally GTK-free so
// it can be unit-tested in isolation; UI code lives in
// internal/ui/bootstrap.
package bootstrap

import (
	"time"

	"github.com/google/uuid"
)

// NodeRole indicates the function of a node in the bootstrapped cluster.
type NodeRole string

const (
	RoleServer NodeRole = "server"
	RoleAgent  NodeRole = "agent"
)

// AuthMethod is the strategy used to authenticate to a node over SSH.
type AuthMethod string

const (
	AuthPassword   AuthMethod = "password"
	AuthPrivateKey AuthMethod = "privateKey"
	AuthAgent      AuthMethod = "agent"
)

// BecomeMethod is the strategy used to gain root privileges on a node.
type BecomeMethod string

const (
	BecomeNone BecomeMethod = "none"
	BecomeSudo BecomeMethod = "sudo"
	BecomeSu   BecomeMethod = "su"
)

// Node describes a target machine for the bootstrap.
type Node struct {
	ID   string
	Role NodeRole

	Host string
	Port int
	User string

	Auth           AuthMethod
	Password       string
	PrivateKeyPath string
	PrivateKeyData []byte

	Become         BecomeMethod
	BecomePassword string

	// Optional friendly label, used in the UI and for k3s --node-name.
	Label string
}

// NewNode returns a Node with a fresh ID and sensible defaults.
func NewNode(role NodeRole) Node {
	return Node{
		ID:     uuid.NewString(),
		Role:   role,
		Port:   22,
		User:   "root",
		Auth:   AuthAgent,
		Become: BecomeNone,
	}
}

// K3sOptions captures the high-level cluster-shape choices the user makes
// on the Intro page. Anything more exotic should be added here so the plan
// generator stays the single source of truth.
type K3sOptions struct {
	ClusterName string

	// Channel is one of "stable", "latest", "testing", or empty (=stable).
	Channel string
	// Version pins to a specific release like "v1.31.4+k3s1". Overrides
	// Channel when set.
	Version string

	// CNI: "flannel" (default), "none". "none" disables k3s' built-in CNI
	// and the user is expected to apply Calico/Cilium manifests manually.
	CNI string

	// Components to disable, e.g. "traefik", "servicelb", "metrics-server".
	DisableComponents []string

	// Networking (advanced).
	ClusterCIDR string
	ServiceCIDR string

	// TLSSANs are additional Subject Alternative Names baked into the
	// kube-apiserver certificate. The wizard auto-adds the server's
	// public host.
	TLSSANs []string

	// LocalBinaryPath, if set, overrides the curl-pipe install with a
	// local k3s binary (air-gapped install).
	LocalBinaryPath string

	// UpdateSystem, when true, runs a full system package upgrade on each
	// node before installing k3s.
	UpdateSystem bool

	// InstallDocker, when true, installs Docker CE via the official
	// convenience script (https://get.docker.com) on each node.
	InstallDocker bool
}

// BootstrapDraft is the in-memory model the wizard mutates as the user
// fills in the form. It is the single source of truth shared between
// pages via a pubsub.Property.
type BootstrapDraft struct {
	Options K3sOptions
	Nodes   []Node

	// Probes is keyed by Node.ID. It is populated by the Probe page.
	Probes map[string]*NodeProbe

	// Plan is populated by the Plan page from Options + Probes.
	Plan *Plan
}

// Server returns the server node, or nil if not yet defined.
func (d *BootstrapDraft) Server() *Node {
	for i := range d.Nodes {
		if d.Nodes[i].Role == RoleServer {
			return &d.Nodes[i]
		}
	}
	return nil
}

// Agents returns all agent nodes in declaration order.
func (d *BootstrapDraft) Agents() []*Node {
	var out []*Node
	for i := range d.Nodes {
		if d.Nodes[i].Role == RoleAgent {
			out = append(out, &d.Nodes[i])
		}
	}
	return out
}

// NodeProbe is the parsed result of running probe.sh on a node.
type NodeProbe struct {
	Distro     string // "ubuntu", "debian", "fedora", "alpine", "rhel", ...
	Version    string
	Arch       string // "amd64", "arm64"
	Kernel     string
	PkgManager string // "apt", "dnf", "yum", "zypper", "pacman", "apk"

	SELinux  string // "Enforcing", "Permissive", "Disabled", ""
	AppArmor bool

	Firewall string // "firewalld", "ufw", "nftables", "iptables", ""

	NetworkInterface string // default route interface, e.g. "eth0", "enp1s0", "wlan0"
	NetworkKind      string // "wired", "wireless", or ""
	NetworkIP        string // first IPv4 address on NetworkInterface

	HasK3s        bool
	K3sVersion    string
	HasContainerd bool
	HasDocker     bool

	SwapEnabled bool
	CgroupV2    bool

	HasModBrNetfilter bool
	HasModOverlay     bool

	FreeDiskMB    int64
	FreeMemoryMB  int64
	CPUCount      int
	IsRoot        bool
	HasSudoNoPass bool

	// Raw is the unparsed key=value output for debugging / display.
	Raw string

	// Warnings flag conditions that the plan can work around (e.g. swap
	// is on; the plan will turn it off). Blockers prevent the install.
	Warnings []string
	Blockers []string
}

// IsBlocked returns true when the probe found a condition the plan
// cannot work around without user intervention.
func (p *NodeProbe) IsBlocked() bool {
	return len(p.Blockers) > 0
}

// StepStatus is the lifecycle state of a single Step within the executor.
type StepStatus string

const (
	StatusPending  StepStatus = "pending"
	StatusRunning  StepStatus = "running"
	StatusDone     StepStatus = "done"
	StatusFailed   StepStatus = "failed"
	StatusSkipped  StepStatus = "skipped"
	StatusCanceled StepStatus = "canceled"
)

// StepEffect is a coarse classification used by the UI to colour the
// "what does this do" chip on each step row.
type StepEffect string

const (
	EffectIdempotent StepEffect = "idempotent" // safe to retry, e.g. mkdir -p
	EffectInstall    StepEffect = "install"    // installs software
	EffectFirewall   StepEffect = "firewall"   // mutates the firewall
	EffectSystem     StepEffect = "system"     // mutates kernel/services
	EffectFile       StepEffect = "file"       // writes a file
	EffectReadOnly   StepEffect = "read-only"  // pure read, e.g. fetch token
)

// Step is a single command (or set of commands joined by &&) executed on
// a single node. The Command field is what is shown to the user in the
// Plan page and what is actually sent to the remote shell — there is no
// hidden wrapping beyond what RequiresRoot adds.
type Step struct {
	ID           string
	Title        string
	Description  string
	Command      string
	RequiresRoot bool
	Effect       StepEffect
	Skip         bool
	// SkipReason is set by the plan generator when a step is pre-skipped
	// based on probe results, e.g. "k3s already installed at this version".
	SkipReason string

	// Status and ExitCode are mutated by the executor; the UI reads them
	// through the Event stream rather than this struct directly.
	Status   StepStatus
	ExitCode int
}

// Plan is the per-node ordered list of Steps the executor will run.
type Plan struct {
	Options   K3sOptions
	NodeSteps map[string][]Step // keyed by Node.ID, in run order
	NodeOrder []string          // server first, then agents in declared order
}

// Event is published by the executor on each lifecycle change or output
// line. The UI drains it on a goroutine.
type Event struct {
	NodeID string
	StepID string
	When   time.Time

	// One of: "step.start", "step.end", "stdout", "stderr", "log".
	Kind string

	Line     string     // for stdout/stderr/log
	Status   StepStatus // for step.end
	ExitCode int        // for step.end
	Err      error      // for step.end on failure
}
