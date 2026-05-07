# Manual test matrix: k3s bootstrap wizard

The unit tests in this package cover plan generation, kubeconfig
rewriting, and probe parsing. End-to-end coverage of the SSH transport
and live executor requires real nodes. Use this matrix when validating
a release.

## Recommended targets

| Distro          | Package mgr | Firewall   | Notes                                 |
|-----------------|-------------|------------|---------------------------------------|
| Ubuntu 24.04    | apt         | ufw        | most common; ensure ufw rules added   |
| Debian 12       | apt         | nftables   | verify nft ruleset is appended        |
| Fedora 41       | dnf         | firewalld  | verify `firewall-cmd --reload` runs   |
| Alpine 3.20     | apk         | iptables   | OpenRC service install                |
| Rocky/Alma 9    | dnf         | firewalld  | SELinux enforcing; must succeed       |

Each target should be tested in two scenarios:

1. **Clean**: fresh OS, nothing installed besides openssh-server.
2. **Dirty**: node already has docker (or k3s of a different version)
   installed; the wizard should detect and surface this in the probe
   page and skip / rewrite the relevant install steps.

## Topologies

- **Single server**: one node, role=server. Fastest smoke test.
- **Server + 2 agents**: exercise token plumbing and node-readiness wait.

## Per-run checklist

1. Welcome window → "Create New Cluster" pill.
2. Intro page: pick channel (`stable`), default CIDRs, disable
   `traefik`. Cluster name something memorable.
3. Nodes page: add server + agents. Click "Test connection" for each
   row; expect green check.
4. Probe page: verify distro/version/arch/firewall reflect reality.
   Warnings should appear for swap-on, SELinux-enforcing, etc.
5. Plan page: expand every step. Confirm:
   - all sudo wrappers are visible,
   - the curl-pipe URL is the official `https://get.k3s.io`,
   - skipped steps are correctly inferred from the probe.
   Edit one command (e.g., add `--debug` to install) and verify it
   round-trips into the apply phase.
6. Apply page: per-node tab streams stdout/stderr live. Sidebar shows
   each step transitioning pending → running → done. Final step is
   "Wait for node Ready".
7. Finish page: "Open Cluster" should switch to a newly opened
   ClusterWindow against the new cluster. "Save kubeconfig…" writes
   a valid file usable by stand-alone `kubectl`.

## Failure modes to verify

- **Wrong SSH password**: Test connection on Nodes page surfaces an
  inline error; wizard cannot advance.
- **Host key mismatch**: second run after node reinstall hard-fails
  with a clear dialog (does not silently accept).
- **k3s install network failure**: Apply page shows the failed step
  with stderr tail; Retry / Edit & Retry / Skip / Abort options work.
- **Abort mid-install**: "Cancel" closes SSH sessions; partial
  install is left as-is (uninstall is best-effort).
