package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	core "github.com/getseabird/seabird/internal/bootstrap"
	"github.com/getseabird/seabird/widget"
	"golang.org/x/crypto/ssh"
)

// ---------- Intro page ---------------------------------------------------

func (w *Wizard) intro() *adw.NavigationPage {
	page := adw.NewPreferencesPage()

	general := adw.NewPreferencesGroup()
	general.SetTitle("Cluster")
	general.SetDescription("High-level shape of the new k3s cluster. Every command the wizard runs on your nodes will be shown — and editable — before it executes.")
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
	rebuild := func() {}

	render := func() *adw.PreferencesPage {
		fresh := adw.NewPreferencesPage()
		d := w.draft.Value()
		for i := range d.Nodes {
			idx := i
			fresh.Add(w.nodeGroup(&d.Nodes[idx], func() {
				w.draft.Pub(d)
				rebuild()
			}, func() {
				if d.Nodes[idx].Role == core.RoleServer {
					return
				}
				d.Nodes = append(d.Nodes[:idx], d.Nodes[idx+1:]...)
				w.draft.Pub(d)
				rebuild()
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
			rebuild()
		})
		add.Add(btn)
		fresh.Add(add)
		return fresh
	}

	bin := adw.NewBin()
	bin.SetChild(render())
	rebuild = func() { bin.SetChild(render()) }

	return w.pageShell("Nodes", "Continue", bin, func() {
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

	shell := w.pageShell("Probe", "Continue", body, func() {
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
	})

	go w.runProbes(banner, results)
	return shell
}

func (w *Wizard) runProbes(banner *adw.Banner, group *adw.PreferencesGroup) {
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
		glib.IdleAdd(func() { w.errorToast(err) })
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
			if rr.err != nil {
				row := adw.NewActionRow()
				row.SetTitle(labelOr(rr.node))
				row.SetSubtitle(fmt.Sprintf("error: %s", rr.err))
				row.AddCSSClass("error")
				group.Add(row)
				return
			}
			draft.Probes[rr.node.ID] = rr.probe
			w.draft.Pub(draft)
			group.Add(probeSummaryRow(rr.node, rr.probe))
			if rr.client != nil {
				_ = rr.client.Close()
			}
		})
	}

	glib.IdleAdd(func() {
		banner.SetTitle("Probe complete")
		banner.SetRevealed(false)
	})
}

func probeSummaryRow(n core.Node, p *core.NodeProbe) *adw.ExpanderRow {
	row := adw.NewExpanderRow()
	row.SetTitle(labelOr(n))
	row.SetSubtitle(fmt.Sprintf("%s %s · %s · %s", p.Distro, p.Version, p.Arch, p.PkgManager))
	if p.IsBlocked() {
		row.AddCSSClass("error")
	} else if len(p.Warnings) > 0 {
		row.AddCSSClass("warning")
	} else {
		row.AddCSSClass("success")
	}

	add := func(title, val string) {
		r := adw.NewActionRow()
		r.SetTitle(title)
		r.SetSubtitle(val)
		row.AddRow(r)
	}
	add("Kernel", p.Kernel)
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
		r.AddCSSClass("warning")
		row.AddRow(r)
	}
	for _, msg := range p.Blockers {
		r := adw.NewActionRow()
		r.SetTitle("Blocker")
		r.SetSubtitle(msg)
		r.AddCSSClass("error")
		row.AddRow(r)
	}
	return row
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

	skip := gtk.NewSwitch()
	skip.SetActive(st.Skip)
	skip.SetVAlign(gtk.AlignCenter)
	skip.ConnectStateSet(func(state bool) bool {
		st.Skip = state
		commit()
		return false
	})
	row.AddSuffix(skip)

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
		status.SetIconName("emblem-ok-symbolic")
		status.SetTitle("Cluster ready")
		status.SetDescription("Your new k3s cluster is up. Open it in Seabird or save the kubeconfig.")
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

	if success {
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
