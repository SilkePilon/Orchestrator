package bootstrap

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed scripts/probe.sh
var probeScript string

// ProbeScript returns the embedded probe script. Exposed so the UI can
// show users exactly what will run when they click "Probe".
func ProbeScript() string {
	return probeScript
}

// Probe runs the embedded read-only probe script on the node and returns
// a parsed NodeProbe along with any warnings or blockers detected.
func Probe(ctx context.Context, c *Client) (*NodeProbe, error) {
	out, stderr, code, err := c.Run(ctx, probeScript)
	if err != nil {
		return nil, fmt.Errorf("probe ssh: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("probe exited %d: %s", code, strings.TrimSpace(stderr))
	}
	p := Parse(out)
	classifyProbe(p)
	return p, nil
}

// Parse turns the raw key=value output of probe.sh into a NodeProbe.
// It is exported so plan_test.go and probe_test.go can drive it from
// fixtures.
func Parse(raw string) *NodeProbe {
	p := &NodeProbe{Raw: raw}
	scan := bufio.NewScanner(strings.NewReader(raw))
	for scan.Scan() {
		line := scan.Text()
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k, v := line[:eq], line[eq+1:]
		switch k {
		case "distro":
			p.Distro = v
		case "version":
			p.Version = v
		case "arch":
			p.Arch = v
		case "kernel":
			p.Kernel = v
		case "pkg":
			p.PkgManager = v
		case "selinux":
			p.SELinux = v
		case "apparmor":
			p.AppArmor = v == "true"
		case "firewall":
			p.Firewall = v
		case "has_k3s":
			p.HasK3s = v == "true"
		case "k3s_version":
			p.K3sVersion = v
		case "has_containerd":
			p.HasContainerd = v == "true"
		case "has_docker":
			p.HasDocker = v == "true"
		case "swap":
			p.SwapEnabled = v == "true"
		case "cgroupv2":
			p.CgroupV2 = v == "true"
		case "mod_br_netfilter":
			p.HasModBrNetfilter = v == "true"
		case "mod_overlay":
			p.HasModOverlay = v == "true"
		case "free_disk_mb":
			p.FreeDiskMB, _ = strconv.ParseInt(v, 10, 64)
		case "free_memory_mb":
			p.FreeMemoryMB, _ = strconv.ParseInt(v, 10, 64)
		case "cpu_count":
			n, _ := strconv.Atoi(v)
			p.CPUCount = n
		case "is_root":
			p.IsRoot = v == "true"
		case "sudo_nopass":
			p.HasSudoNoPass = v == "true"
		}
	}
	return p
}

// classifyProbe populates the Warnings/Blockers slices.
func classifyProbe(p *NodeProbe) {
	switch p.Arch {
	case "amd64", "arm64":
	case "":
		p.Blockers = append(p.Blockers, "could not determine CPU architecture")
	default:
		p.Blockers = append(p.Blockers, fmt.Sprintf("unsupported architecture: %s", p.Arch))
	}
	if p.FreeDiskMB > 0 && p.FreeDiskMB < 2048 {
		p.Warnings = append(p.Warnings,
			fmt.Sprintf("low free disk on /var/lib (%d MB) — k3s recommends ≥2 GB", p.FreeDiskMB))
	}
	if p.FreeMemoryMB > 0 && p.FreeMemoryMB < 512 {
		p.Warnings = append(p.Warnings,
			fmt.Sprintf("low free memory (%d MB) — k3s recommends ≥512 MB", p.FreeMemoryMB))
	}
	if p.SwapEnabled {
		p.Warnings = append(p.Warnings, "swap is enabled — k3s will run with swap; the plan will disable it")
	}
	if !p.CgroupV2 {
		p.Warnings = append(p.Warnings, "cgroup v2 not detected — k3s may still work but this is unusual on modern distros")
	}
	if p.HasK3s {
		p.Warnings = append(p.Warnings,
			fmt.Sprintf("k3s is already installed (%s) — it will be reconfigured / upgraded", p.K3sVersion))
	}
	if p.HasDocker && !p.HasContainerd {
		p.Warnings = append(p.Warnings, "Docker is installed; k3s ships its own containerd and will not use Docker")
	}
}
