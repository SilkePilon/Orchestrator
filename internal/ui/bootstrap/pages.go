package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	core "github.com/SilkePilon/Orchestrator/internal/bootstrap"
	"github.com/SilkePilon/Orchestrator/widget"
	"golang.org/x/crypto/ssh"
)

// ---------- Intro page ---------------------------------------------------

func (w *Wizard) intro() *adw.NavigationPage {
	page := adw.NewPreferencesPage()

	general := adw.NewPreferencesGroup()
	general.SetTitle("Cluster")
	general.SetDescription("High-level shape of the new k3s cluster. Every command the wizard runs on your nodes will be shown and editable before it executes.")
	page.Add(general)

	d := w.draft.Value()

	name := adw.NewEntryRow()
	name.SetTitle("Cluster name")
	name.SetText(d.Options.ClusterName)
	general.Add(name)

	channel := adw.NewComboRow()
	channel.SetTitle("k3s channel")
	channelModel := gtk.NewStringList([]string{"stable", "latest", "testing"})
	channel.SetModel(channelModel)
	switch d.Options.Channel {
	case "latest":
		channel.SetSelected(1)
	case "testing":
		channel.SetSelected(2)
	default:
		channel.SetSelected(0)
	}
	general.Add(channel)

	version := adw.NewEntryRow()
	version.SetTitle("Pin version (optional)")
	version.SetText(d.Options.Version)
	general.Add(version)

	cni := adw.NewComboRow()
	cni.SetTitle("CNI")
	cniModel := gtk.NewStringList([]string{"flannel (built-in)", "none (apply manually)"})
	cni.SetModel(cniModel)
	if d.Options.CNI == "none" {
		cni.SetSelected(1)
	}
	general.Add(cni)

	advanced := adw.NewPreferencesGroup()
	advanced.SetTitle("Advanced")
	page.Add(advanced)

	disable := adw.NewEntryRow()
	disable.SetTitle("Disable components (comma-separated)")
	disable.SetText(strings.Join(d.Options.DisableComponents, ","))
	advanced.Add(disable)

	clusterCIDR := adw.NewEntryRow()
	clusterCIDR.SetTitle("Cluster CIDR")
	clusterCIDR.SetText(d.Options.ClusterCIDR)
	advanced.Add(clusterCIDR)

	serviceCIDR := adw.NewEntryRow()
	serviceCIDR.SetTitle("Service CIDR")
	serviceCIDR.SetText(d.Options.ServiceCIDR)
	advanced.Add(serviceCIDR)

	tlsSAN := adw.NewEntryRow()
	tlsSAN.SetTitle("Extra TLS SANs (comma-separated)")
	tlsSAN.SetText(strings.Join(d.Options.TLSSANs, ","))
	advanced.Add(tlsSAN)

	commit := func() {
		d := w.draft.Value()
		d.Options.ClusterName = strings.TrimSpace(name.Text())
		d.Options.Channel = []string{"stable", "latest", "testing"}[channel.Selected()]
		d.Options.Version = strings.TrimSpace(version.Text())
		d.Options.CNI = []string{"flannel", "none"}[cni.Selected()]
		d.Options.DisableComponents = splitTrim(disable.Text(), ",")
		d.Options.ClusterCIDR = strings.TrimSpace(clusterCIDR.Text())
		d.Options.ServiceCIDR = strings.TrimSpace(serviceCIDR.Text())
		d.Options.TLSSANs = splitTrim(tlsSAN.Text(), ",")
		w.draft.Pub(d)
	}

	return w.pageShell("Cluster Bootstrap", "Continue", page, func() {
		commit()
		if w.draft.Value().Options.ClusterName == "" {
			w.errorToast(fmt.Errorf("cluster name is required"))
			return
		}
		w.push(w.nodesPage())
	})
}

// ---------- Nodes page --------------------------------------------------

func (w *Wizard) nodesPage() *adw.NavigationPage {
	return w.nodesPageWithContinue("Continue", func() {
		d := w.draft.Value()
		for _, n := range d.Nodes {
			if n.Host == "" {
				w.errorToast(fmt.Errorf("node %q has no host", labelOr(n)))
				return
			}
			if n.User == "" {
				w.errorToast(fmt.Errorf("node %q has no user", labelOr(n)))
				return
			}
		}
		w.push(w.probePage())
	})
}

func (w *Wizard) nodesPageWithContinue(continueLabel string, onContinue func()) *adw.NavigationPage {
	rebuild := func(scrollToBottom bool) {}

	render := func() *adw.PreferencesPage {
		fresh := adw.NewPreferencesPage()
		d := w.draft.Value()
		for i := range d.Nodes {
			idx := i
			fresh.Add(w.nodeGroup(&d.Nodes[idx], func() {
				w.draft.Pub(d)
			}, func() {
				if d.Nodes[idx].Role == core.RoleServer {
					return
				}
				d.Nodes = append(d.Nodes[:idx], d.Nodes[idx+1:]...)
				w.draft.Pub(d)
				rebuild(false)
			}))
		}
		add := adw.NewPreferencesGroup()
		btn := gtk.NewButtonWithLabel("Add agent node")
		btn.SetHAlign(gtk.AlignCenter)
		btn.AddCSSClass("pill")
		btn.ConnectClicked(func() {
			d := w.draft.Value()
			n := core.NewNode(core.RoleAgent)
			n.Label = fmt.Sprintf("agent-%d", len(d.Agents())+1)
			d.Nodes = append(d.Nodes, n)
			w.draft.Pub(d)
			rebuild(true)
		})
		add.Add(btn)
		fresh.Add(add)
		return fresh
	}

	bin := adw.NewBin()
	bin.SetChild(render())
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(bin)
	rebuild = func(scrollToBottom bool) {
		value := scroll.VAdjustment().Value()
		bin.SetChild(render())
		glib.IdleAdd(func() {
			adj := scroll.VAdjustment()
			if scrollToBottom {
				adj.SetValue(adj.Upper() - adj.PageSize())
				return
			}
			adj.SetValue(value)
		})
	}

	return w.pageShellWithHeaderActions("Nodes", continueLabel, scroll, onContinue,
		w.nodesImportButton(rebuild),
		w.nodesExportButton(),
	)
}

// nodeGroup is one PreferencesGroup form for editing a Node in place.
func (w *Wizard) nodeGroup(n *core.Node, commit func(), remove func()) *adw.PreferencesGroup {
	group := adw.NewPreferencesGroup()
	if n.Role == core.RoleServer {
		group.SetTitle("Server node")
		group.SetDescription("Runs the k3s control plane and the kube-apiserver.")
	} else {
		group.SetTitle("Agent node")
		rm := gtk.NewButtonFromIconName("user-trash-symbolic")
		rm.AddCSSClass("flat")
		rm.ConnectClicked(remove)
		group.SetHeaderSuffix(rm)
	}

	label := adw.NewEntryRow()
	label.SetTitle("Label (--node-name)")
	label.SetText(n.Label)
	label.ConnectChanged(func() { n.Label = strings.TrimSpace(label.Text()); commit() })
	group.Add(label)

	host := adw.NewEntryRow()
	host.SetTitle("Host or IP")
	host.SetText(n.Host)
	host.ConnectChanged(func() { n.Host = strings.TrimSpace(host.Text()); commit() })
	group.Add(host)

	port := adw.NewSpinRow(gtk.NewAdjustment(float64(n.Port), 1, 65535, 1, 0, 0), 1, 0)
	port.SetTitle("SSH port")
	port.ConnectChanged(func() { n.Port = int(port.Value()); commit() })
	group.Add(port)

	user := adw.NewEntryRow()
	user.SetTitle("SSH user")
	user.SetText(n.User)
	user.ConnectChanged(func() { n.User = strings.TrimSpace(user.Text()); commit() })
	group.Add(user)

	auth := adw.NewComboRow()
	auth.SetTitle("Auth method")
	auth.SetModel(gtk.NewStringList([]string{"ssh-agent", "Private key file", "Password"}))
	switch n.Auth {
	case core.AuthPrivateKey:
		auth.SetSelected(1)
	case core.AuthPassword:
		auth.SetSelected(2)
	default:
		auth.SetSelected(0)
	}
	group.Add(auth)

	keyPath := adw.NewEntryRow()
	keyPath.SetTitle("Private key path")
	keyPath.SetText(n.PrivateKeyPath)
	keyPath.ConnectChanged(func() { n.PrivateKeyPath = strings.TrimSpace(keyPath.Text()); commit() })
	keyPicker := gtk.NewButtonFromIconName("document-open-symbolic")
	keyPicker.AddCSSClass("flat")
	keyPicker.SetTooltipText("Select private key file")
	keyPicker.ConnectClicked(func() {
		fileChooser := gtk.NewFileChooserNative("Select private key", w.parentWindow(), gtk.FileChooserActionOpen, "Open", "Cancel")
		if home, err := os.UserHomeDir(); err == nil {
			sshDir := filepath.Join(home, ".ssh")
			if info, err := os.Stat(sshDir); err == nil && info.IsDir() {
				_ = fileChooser.SetCurrentFolder(gio.NewFileForPath(sshDir))
			}
		}
		fileChooser.ConnectResponse(func(responseId int) {
			if responseId == int(gtk.ResponseAccept) && fileChooser.File() != nil {
				selected := fileChooser.File().Path()
				if strings.HasSuffix(selected, ".pub") {
					privatePath := strings.TrimSuffix(selected, ".pub")
					if info, err := os.Stat(privatePath); err == nil && !info.IsDir() {
						selected = privatePath
					} else {
						w.errorToast(fmt.Errorf("select the private key file, not %s", filepath.Base(selected)))
						return
					}
				}
				keyPath.SetText(selected)
			}
		})
		fileChooser.Show()
	})
	keyPath.AddSuffix(keyPicker)
	group.Add(keyPath)

	password := adw.NewPasswordEntryRow()
	password.SetText(n.Password)
	password.ConnectChanged(func() { n.Password = password.Text(); commit() })
	group.Add(password)

	updateAuthRows := func() {
		switch auth.Selected() {
		case 0: // ssh-agent
			keyPath.SetVisible(false)
			password.SetVisible(false)
		case 1: // private key
			keyPath.SetVisible(true)
			password.SetVisible(true)
			password.SetTitle("Key passphrase (optional)")
		case 2: // password
			keyPath.SetVisible(false)
			password.SetVisible(true)
			password.SetTitle("Password")
		}
	}
	updateAuthRows()

	auth.Connect("notify::selected", func() {
		switch auth.Selected() {
		case 0:
			n.Auth = core.AuthAgent
		case 1:
			n.Auth = core.AuthPrivateKey
		case 2:
			n.Auth = core.AuthPassword
		}
		updateAuthRows()
		commit()
	})

	become := adw.NewComboRow()
	become.SetTitle("Become root via")
	become.SetModel(gtk.NewStringList([]string{"none (already root)", "sudo", "su"}))
	switch n.Become {
	case core.BecomeSudo:
		become.SetSelected(1)
	case core.BecomeSu:
		become.SetSelected(2)
	default:
		become.SetSelected(0)
	}
	group.Add(become)

	becomePass := adw.NewPasswordEntryRow()
	becomePass.SetTitle("sudo password")
	becomePass.SetText(n.BecomePassword)
	becomePass.ConnectChanged(func() { n.BecomePassword = becomePass.Text(); commit() })
	group.Add(becomePass)

	updateBecomeRows := func() {
		switch become.Selected() {
		case 0:
			becomePass.SetVisible(false)
		case 1:
			becomePass.SetVisible(true)
			becomePass.SetTitle("sudo password (optional)")
		case 2:
			becomePass.SetVisible(true)
			becomePass.SetTitle("root password")
		}
	}
	updateBecomeRows()

	become.Connect("notify::selected", func() {
		switch become.Selected() {
		case 0:
			n.Become = core.BecomeNone
		case 1:
			n.Become = core.BecomeSudo
		case 2:
			n.Become = core.BecomeSu
		}
		updateBecomeRows()
		commit()
	})

	return group
}

// ---------- Probe page --------------------------------------------------

func (w *Wizard) probePage() *adw.NavigationPage {
	body := gtk.NewBox(gtk.OrientationVertical, 12)
	body.SetMarginTop(12)
	body.SetMarginBottom(12)
	body.SetMarginStart(12)
	body.SetMarginEnd(12)

	banner := adw.NewBanner("Inspecting nodes…")
	banner.SetRevealed(true)
	body.Append(banner)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	body.Append(scroll)

	page := adw.NewPreferencesPage()
	scroll.SetChild(page)

	results := adw.NewPreferencesGroup()
	page.Add(results)

	reprobe := gtk.NewButtonFromIconName("view-refresh-symbolic")
	reprobe.AddCSSClass("flat")
	reprobe.SetTooltipText("Re-probe nodes")

	startProbe := func() {
		reprobe.SetSensitive(false)
		banner.SetTitle("Inspecting nodes…")
		banner.SetRevealed(true)
		page.Remove(results)
		results = adw.NewPreferencesGroup()
		page.Add(results)
		draft := w.draft.Value()
		if draft.Probes == nil {
			draft.Probes = map[string]*core.NodeProbe{}
		}
		for _, node := range draft.Nodes {
			delete(draft.Probes, node.ID)
		}
		w.draft.Pub(draft)
		rows := map[string]*probeRowState{}
		for _, node := range draft.Nodes {
			row := probePendingRow(node)
			rows[node.ID] = row
			results.Add(row.row)
		}
		go w.runProbes(banner, rows, func() { reprobe.SetSensitive(true) })
	}
	reprobe.ConnectClicked(startProbe)

	shell := w.pageShellWithHeaderActions("Probe", "Continue", body, func() {
		d := w.draft.Value()
		for _, n := range d.Nodes {
			p, ok := d.Probes[n.ID]
			if !ok || p == nil {
				w.errorToast(fmt.Errorf("probe for %s not yet finished", labelOr(n)))
				return
			}
			if p.IsBlocked() {
				w.errorToast(fmt.Errorf("%s has unresolved blockers; cannot continue", labelOr(n)))
				return
			}
		}
		w.push(w.planPage())
	}, reprobe)

	startProbe()
	return shell
}

func (w *Wizard) runProbes(banner *adw.Banner, rows map[string]*probeRowState, done func()) {
	d := w.draft.Value()

	type result struct {
		node   core.Node
		probe  *core.NodeProbe
		client *core.Client
		err    error
	}
	results := make(chan result, len(d.Nodes))

	store, err := core.DefaultKnownHosts()
	if err != nil {
		glib.IdleAdd(func() {
			w.errorToast(err)
			for _, row := range rows {
				row.showError(err)
			}
			if done != nil {
				done()
			}
		})
		return
	}

	for _, n := range d.Nodes {
		n := n
		go func() {
			c, err := core.Dial(w.ctx, n, store, w.makeHostKeyPrompt())
			if err != nil {
				results <- result{node: n, err: err}
				return
			}
			p, err := core.Probe(w.ctx, c)
			results <- result{node: n, probe: p, client: c, err: err}
		}()
	}

	for i := 0; i < len(d.Nodes); i++ {
		r := <-results
		rr := r
		glib.IdleAdd(func() {
			draft := w.draft.Value()
			row := rows[rr.node.ID]
			if row == nil {
				return
			}
			if rr.err != nil {
				if rr.client != nil {
					_ = rr.client.Close()
				}
				row.showError(rr.err)
				return
			}
			draft.Probes[rr.node.ID] = rr.probe
			w.draft.Pub(draft)
			row.showProbe(rr.node, rr.probe)
			if rr.client != nil {
				_ = rr.client.Close()
			}
		})
	}

	glib.IdleAdd(func() {
		banner.SetTitle("Probe complete")
		banner.SetRevealed(false)
		if done != nil {
			done()
		}
	})
}

type probeRowState struct {
	row    *adw.ExpanderRow
	status gtk.Widgetter
}

func probePendingRow(n core.Node) *probeRowState {
	row := adw.NewExpanderRow()
	row.SetTitle(labelOr(n))
	row.SetSubtitle(fmt.Sprintf("%s@%s", n.User, n.Host))
	row.SetEnableExpansion(false)
	row.SetExpanded(false)
	state := &probeRowState{row: row}
	state.setStatus(probeLoadingBadge())
	return state
}

func (s *probeRowState) setStatus(status gtk.Widgetter) {
	if s.status != nil {
		s.row.Remove(s.status)
	}
	s.status = status
	s.row.AddSuffix(status)
}

func (s *probeRowState) showError(err error) {
	s.row.SetSubtitle(err.Error())
	s.row.SetEnableExpansion(false)
	s.row.SetExpanded(false)
	s.setStatus(probeStatusBadge("cross-small-symbolic", "Failed", "error"))
}

func (s *probeRowState) showProbe(n core.Node, p *core.NodeProbe) {
	s.row.SetSubtitle(fmt.Sprintf("%s %s · %s · %s", p.Distro, p.Version, p.Arch, p.PkgManager))
	s.row.SetEnableExpansion(true)
	if p.IsBlocked() {
		s.setStatus(probeStatusBadge("cross-small-symbolic", "Blocked", "error"))
	} else if len(p.Warnings) > 0 {
		s.setStatus(probeStatusBadge("dialog-warning-symbolic", "Warnings", "warning"))
	} else {
		s.setStatus(probeStatusBadge("verified-checkmark-symbolic", "Ready", "success"))
	}
	addProbeDetails(s.row, p)
	s.row.SetExpanded(false)
}

func addProbeDetails(row *adw.ExpanderRow, p *core.NodeProbe) {
	add := func(title, val string) {
		r := adw.NewActionRow()
		r.SetTitle(title)
		r.SetSubtitle(val)
		row.AddRow(r)
	}
	add("Kernel", p.Kernel)
	add("Network", networkSummary(p))
	add("Firewall", emptyDash(p.Firewall))
	add("Existing k3s", boolStr(p.HasK3s, p.K3sVersion))
	add("Existing containerd", boolStr(p.HasContainerd, ""))
	add("Existing Docker", boolStr(p.HasDocker, ""))
	add("Swap", boolStr(p.SwapEnabled, "(will disable)"))
	add("Cgroup v2", boolStr(p.CgroupV2, ""))
	add("Resources", fmt.Sprintf("%d CPU · %d MB RAM · %d MB disk free on /var/lib/rancher", p.CPUCount, p.FreeMemoryMB, p.FreeDiskMB))

	for _, msg := range p.Warnings {
		r := adw.NewActionRow()
		r.SetTitle("Warning")
		r.SetSubtitle(msg)
		row.AddRow(r)
	}
	for _, msg := range p.Blockers {
		r := adw.NewActionRow()
		r.SetTitle("Blocker")
		r.SetSubtitle(msg)
		row.AddRow(r)
	}
}

func networkSummary(p *core.NodeProbe) string {
	if p.NetworkInterface == "" && p.NetworkIP == "" {
		return "—"
	}

	var parts []string
	if p.NetworkInterface != "" {
		iface := p.NetworkInterface
		if p.NetworkKind != "" {
			iface = fmt.Sprintf("%s (%s)", iface, p.NetworkKind)
		}
		parts = append(parts, iface)
	}
	if p.NetworkIP != "" {
		parts = append(parts, p.NetworkIP)
	}
	return strings.Join(parts, " · ")
}

func probeLoadingBadge() *gtk.Box {
	badge := gtk.NewBox(gtk.OrientationHorizontal, 4)
	badge.SetVAlign(gtk.AlignCenter)
	badge.AddCSSClass("pill")
	spinner := gtk.NewSpinner()
	spinner.SetSizeRequest(10, 10)
	spinner.AddCSSClass("probe-badge-spinner")
	spinner.Start()
	badge.Append(spinner)
	badge.Append(gtk.NewLabel("Probing"))
	return badge
}

func probeStatusBadge(iconName, label, style string) *gtk.Box {
	badge := gtk.NewBox(gtk.OrientationHorizontal, 4)
	badge.SetVAlign(gtk.AlignCenter)
	badge.AddCSSClass("pill")
	icon := gtk.NewImageFromIconName(iconName)
	icon.SetPixelSize(12)
	icon.AddCSSClass("probe-badge-icon")
	icon.AddCSSClass(style)
	badge.Append(icon)
	badge.Append(gtk.NewLabel(label))
	return badge
}

// makeHostKeyPrompt returns a HostKeyPrompt that opens an Adwaita
// alert dialog to ask the user whether to trust an unknown host key.
func (w *Wizard) makeHostKeyPrompt() core.HostKeyPrompt {
	return func(ctx context.Context, addr string, key ssh.PublicKey) (core.HostKeyDecision, error) {
		ch := make(chan core.HostKeyDecision, 1)
		glib.IdleAdd(func() {
			fp := ssh.FingerprintSHA256(key)
			d := adw.NewAlertDialog("Trust host key?",
				fmt.Sprintf("New SSH host key for %s\nFingerprint: %s\nType: %s", addr, fp, key.Type()))
			d.AddResponse("reject", "Reject")
			d.AddResponse("accept", "Accept and remember")
			d.SetResponseAppearance("accept", adw.ResponseSuggested)
			d.SetDefaultResponse("reject")
			d.ConnectResponse(func(resp string) {
				if resp == "accept" {
					ch <- core.HostKeyAccept
				} else {
					ch <- core.HostKeyReject
				}
			})
			d.Present(w.parentWindow())
		})
		return <-ch, nil
	}
}

// ---------- Plan page ---------------------------------------------------

func (w *Wizard) planPage() *adw.NavigationPage {
	d := w.draft.Value()
	plan, err := core.BuildPlan(d)
	if err != nil {
		w.errorToast(err)
		return w.pageShell("Plan", "", gtk.NewLabel("Failed to build plan"), nil)
	}
	d.Plan = plan
	w.draft.Pub(d)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)

	for _, nodeID := range plan.NodeOrder {
		var node core.Node
		for _, n := range d.Nodes {
			if n.ID == nodeID {
				node = n
				break
			}
		}
		group := adw.NewPreferencesGroup()
		group.SetTitle(labelOr(node))
		group.SetDescription(fmt.Sprintf("%s — %s@%s", node.Role, node.User, node.Host))
		page.Add(group)

		steps := plan.NodeSteps[nodeID]
		for i := range steps {
			idx := i
			st := &steps[idx]
			group.Add(stepExpanderRow(st, func() { plan.NodeSteps[nodeID][idx] = *st }))
		}
	}

	return w.pageShell("Plan & Review", "Apply", scroll, func() {
		w.push(w.applyPage())
	})
}

func (w *Wizard) uninstallPlanPage() *adw.NavigationPage {
	d := w.draft.Value()
	plan, err := core.BuildUninstallPlan(d)
	if err != nil {
		w.errorToast(err)
		return w.pageShell("Uninstall Plan", "", gtk.NewLabel("Failed to build uninstall plan"), nil)
	}
	d.Plan = plan
	w.draft.Pub(d)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)

	for _, nodeID := range plan.NodeOrder {
		var node core.Node
		for _, n := range d.Nodes {
			if n.ID == nodeID {
				node = n
				break
			}
		}
		group := adw.NewPreferencesGroup()
		group.SetTitle(labelOr(node))
		group.SetDescription(fmt.Sprintf("%s — %s@%s", node.Role, node.User, node.Host))
		page.Add(group)

		steps := plan.NodeSteps[nodeID]
		for i := range steps {
			idx := i
			st := &steps[idx]
			group.Add(stepExpanderRow(st, func() { plan.NodeSteps[nodeID][idx] = *st }))
		}
	}

	return w.pageShell("Uninstall Plan", "Uninstall", scroll, func() {
		dialog := adw.NewMessageDialog(w.parentWindow(), "Uninstall cluster from nodes?", "This will remove k3s, related Kubernetes data, CNI state, Orchestrator-created module config, and best-effort firewall rules from the listed nodes.")
		dialog.AddResponse("cancel", "Cancel")
		dialog.AddResponse("uninstall", "Uninstall")
		dialog.SetResponseAppearance("uninstall", adw.ResponseDestructive)
		dialog.Present()
		dialog.ConnectResponse(func(response string) {
			if response == "uninstall" {
				w.push(w.applyPage())
			}
		})
	})
}

func stepExpanderRow(st *core.Step, commit func()) *adw.ExpanderRow {
	row := adw.NewExpanderRow()
	row.SetTitle(st.Title)
	subtitle := string(st.Effect)
	if st.RequiresRoot {
		subtitle += " · root"
	}
	if st.SkipReason != "" {
		subtitle += " · " + st.SkipReason
	}
	row.SetSubtitle(subtitle)

	runState := gtk.NewLabel("")
	runState.SetVAlign(gtk.AlignCenter)
	runState.AddCSSClass("dim-label")
	setRunState := func() {
		if st.Skip {
			runState.SetText("Will skip")
			return
		}
		runState.SetText("Will run")
	}
	setRunState()

	runSwitch := gtk.NewSwitch()
	runSwitch.SetActive(!st.Skip)
	runSwitch.SetVAlign(gtk.AlignCenter)
	runSwitch.SetTooltipText("Run this step when enabled; skip it when disabled")
	runSwitch.ConnectStateSet(func(state bool) bool {
		st.Skip = !state
		setRunState()
		commit()
		return false
	})

	runToggle := gtk.NewBox(gtk.OrientationHorizontal, 6)
	runToggle.SetVAlign(gtk.AlignCenter)
	runToggle.Append(runState)
	runToggle.Append(runSwitch)
	row.AddSuffix(runToggle)

	body := gtk.NewBox(gtk.OrientationVertical, 6)
	body.SetMarginTop(6)
	body.SetMarginBottom(6)
	body.SetMarginStart(12)
	body.SetMarginEnd(12)

	if st.Description != "" {
		desc := gtk.NewLabel(st.Description)
		desc.SetXAlign(0)
		desc.SetWrap(true)
		desc.AddCSSClass("dim-label")
		body.Append(desc)
	}

	scroll := gtk.NewScrolledWindow()
	scroll.SetMinContentHeight(80)
	scroll.SetMaxContentHeight(280)
	view := gtk.NewTextView()
	view.SetMonospace(true)
	view.SetEditable(true)
	view.SetWrapMode(gtk.WrapWordChar)
	buf := view.Buffer()
	buf.SetText(st.Command)
	buf.ConnectChanged(func() {
		start, end := buf.Bounds()
		st.Command = buf.Text(start, end, true)
		commit()
	})
	scroll.SetChild(view)
	body.Append(scroll)

	wrap := adw.NewActionRow()
	wrap.SetActivatable(false)
	wrap.SetChild(body)
	row.AddRow(wrap)
	return row
}

// ---------- Finish page ------------------------------------------------

func (w *Wizard) finishPage(success bool, kubeconfigYAML string, finalErr error) *adw.NavigationPage {
	status := adw.NewStatusPage()
	if success {
		status.SetIconName("verified-checkmark-symbolic")
		status.SetTitle(w.finishSuccessTitle)
		status.SetDescription(w.finishSuccessDescription)
	} else if errors.Is(finalErr, context.Canceled) {
		status.SetIconName("process-stop-symbolic")
		status.SetTitle("Bootstrap canceled")
		status.SetDescription("Apply was aborted before the cluster finished bootstrapping.")
	} else {
		status.SetIconName("dialog-error-symbolic")
		status.SetTitle("Bootstrap failed")
		if finalErr != nil {
			status.SetDescription(finalErr.Error())
		} else {
			status.SetDescription("Review the apply log and try again.")
		}
	}

	actions := gtk.NewBox(gtk.OrientationHorizontal, 12)
	actions.SetHAlign(gtk.AlignCenter)
	status.SetChild(actions)

	if success && kubeconfigYAML == "" {
		done := gtk.NewButtonWithLabel("Done")
		done.AddCSSClass("pill")
		done.AddCSSClass("suggested-action")
		done.ConnectClicked(func() {
			if parent, ok := w.Parent().(*adw.NavigationView); ok {
				parent.Pop()
			}
			if w.onApplySuccess != nil {
				w.onApplySuccess()
			}
		})
		actions.Append(done)
	}

	if success && kubeconfigYAML != "" {
		open := gtk.NewButtonWithLabel("Open Cluster")
		open.AddCSSClass("pill")
		open.AddCSSClass("suggested-action")
		open.ConnectClicked(func() {
			if w.onFinish != nil {
				w.onFinish(w.ctx, w.draft.Value(), kubeconfigYAML)
			}
		})
		actions.Append(open)

		save := gtk.NewButtonWithLabel("Save kubeconfig…")
		save.AddCSSClass("pill")
		save.ConnectClicked(func() {
			fc := gtk.NewFileChooserNative("Save kubeconfig", w.parentWindow(),
				gtk.FileChooserActionSave, "Save", "Cancel")
			fc.SetCurrentName("k3s.kubeconfig")
			fc.ConnectResponse(func(id int) {
				if id != int(gtk.ResponseAccept) {
					return
				}
				if err := writeFile(fc.File().Path(), kubeconfigYAML); err != nil {
					widget.ShowErrorDialog(w.ctx, "Could not save kubeconfig", err)
				}
			})
			fc.Show()
		})
		actions.Append(save)
	} else {
		back := gtk.NewButtonWithLabel("Back to Plan")
		back.AddCSSClass("pill")
		back.ConnectClicked(func() {
			// Pop back to the plan page (two pops: finish, apply).
			w.nav.Pop()
			w.nav.Pop()
		})
		actions.Append(back)
	}

	return w.pageShell("Finish", "", status, nil)
}

// ---------- helpers ----------------------------------------------------

func labelOr(n core.Node) string {
	if n.Label != "" {
		return n.Label
	}
	if n.Host != "" {
		return n.Host
	}
	return string(n.Role)
}

func splitTrim(s, sep string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func boolStr(v bool, extra string) string {
	if v {
		if extra == "" {
			return "yes"
		}
		return "yes " + extra
	}
	return "no"
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
