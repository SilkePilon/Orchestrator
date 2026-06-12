package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	core "github.com/SilkePilon/Orchestrator/internal/bootstrap"
	"github.com/SilkePilon/Orchestrator/internal/pubsub"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// NewAddNodeWizard returns a wizard that joins a single new agent node to an
// existing bootstrapped cluster. onSuccess is called after the apply step
// finishes successfully so the caller can update stored BootstrapRecord
// metadata.
func NewAddNodeWizard(ctx context.Context, state *common.State, cluster api.ClusterPreferences, onSuccess func(newNode core.Node)) *Wizard {
	w := &Wizard{
		ctx:                      ctx,
		state:                    state,
		draft:                    pubsub.NewProperty(addNodeDraft(cluster)),
		requireKubeconfig:        false,
		finishSuccessTitle:       "Node added",
		finishSuccessDescription: "The new agent node has joined the cluster.",
	}

	w.onApplySuccess = func() {
		d := w.draft.Value()
		for _, n := range d.Agents() {
			onSuccess(*n)
		}
	}

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	w.NavigationPage = adw.NewNavigationPage(box, "Add Node")

	w.toast = adw.NewToastOverlay()
	box.Append(w.toast)

	w.nav = adw.NewNavigationView()
	w.toast.SetChild(w.nav)
	w.nav.Add(w.addNodePage())

	return w
}

// addNodeDraft builds a BootstrapDraft pre-populated with the existing
// server node (SSH credentials restored from the BootstrapRecord) and a
// fresh default agent node that the user will configure.
func addNodeDraft(cluster api.ClusterPreferences) *core.BootstrapDraft {
	d := &core.BootstrapDraft{
		Options: core.K3sOptions{ClusterName: cluster.Name},
		Probes:  map[string]*core.NodeProbe{},
	}

	// Restore server node from stored record.
	var serverNode core.Node
	if cluster.Bootstrap != nil {
		for _, rec := range cluster.Bootstrap.Nodes {
			if rec.Role == string(core.RoleServer) {
				serverNode = core.NewNode(core.RoleServer)
				serverNode.Host = rec.Host
				serverNode.Port = rec.Port
				if serverNode.Port == 0 {
					serverNode.Port = 22
				}
				serverNode.User = rec.User
				if serverNode.User == "" {
					serverNode.User = "root"
				}
				serverNode.Auth = authFromRecord(rec.Auth)
				serverNode.PrivateKeyPath = rec.PrivateKeyPath
				serverNode.Become = becomeFromRecord(rec.Become)
				serverNode.Label = rec.Label
				break
			}
		}
		// Fall back to the legacy ServerHost field when Nodes is empty.
		if serverNode.Host == "" && cluster.Bootstrap.ServerHost != "" {
			serverNode = core.NewNode(core.RoleServer)
			serverNode.Host = cluster.Bootstrap.ServerHost
			serverNode.Label = "server-1"
		}
	}
	if serverNode.Host == "" {
		serverNode = core.NewNode(core.RoleServer)
		serverNode.Label = "server-1"
	}
	d.Nodes = append(d.Nodes, serverNode)

	// Fresh agent node for the user to configure.
	agent := core.NewNode(core.RoleAgent)
	existingAgentCount := 0
	if cluster.Bootstrap != nil {
		for _, rec := range cluster.Bootstrap.Nodes {
			if rec.Role == string(core.RoleAgent) {
				existingAgentCount++
			}
		}
		// Fall back to AgentHosts count when Nodes is empty.
		if existingAgentCount == 0 {
			existingAgentCount = len(cluster.Bootstrap.AgentHosts)
		}
	}
	agent.Label = fmt.Sprintf("agent-%d", existingAgentCount+1)
	d.Nodes = append(d.Nodes, agent)

	return d
}

// addNodePage is the first page of the wizard. It shows two node forms:
// the existing server (SSH credentials for token retrieval only) and the
// new agent node that will join the cluster.
func (w *Wizard) addNodePage() *adw.NavigationPage {
	d := w.draft.Value()

	page := adw.NewPreferencesPage()

	// -- Existing server form --
	srvIdx := -1
	for i := range d.Nodes {
		if d.Nodes[i].Role == core.RoleServer {
			srvIdx = i
			break
		}
	}
	if srvIdx >= 0 {
		srvGroup := w.nodeGroup(&d.Nodes[srvIdx], func() { w.draft.Pub(d) }, func() {})
		srvGroup.SetTitle("Existing server (token source)")
		srvGroup.SetDescription("SSH credentials used only to read the cluster join token. The server will not be changed.")
		page.Add(srvGroup)
	}

	// -- New agent node form --
	for i := range d.Nodes {
		if d.Nodes[i].Role == core.RoleAgent {
			idx := i
			agentGroup := w.nodeGroup(&d.Nodes[idx], func() { w.draft.Pub(d) }, func() {})
			agentGroup.SetTitle("New agent node")
			agentGroup.SetDescription("This node will be installed with k3s agent and will join the existing cluster.")
			page.Add(agentGroup)
			break // only one new agent node per wizard run
		}
	}

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(page)

	return w.pageShell("Add Node", "Continue", scroll, func() {
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
		w.push(w.addNodeProbePage())
	})
}

// addNodeProbePage probes only the new agent node(s). The existing server is
// not probed — it is already running and the plan only reads its token.
func (w *Wizard) addNodeProbePage() *adw.NavigationPage {
	pageBox := gtk.NewBox(gtk.OrientationVertical, 0)
	navPage := adw.NewNavigationPage(pageBox, "Probe")

	titleWidget := adw.NewWindowTitle("Probe", "Inspecting new node…")

	reprobe := gtk.NewButtonFromIconName("view-refresh-symbolic")
	reprobe.AddCSSClass("flat")
	reprobe.SetTooltipText("Re-probe node")

	continueBtn := gtk.NewButtonWithLabel("Continue")
	continueBtn.AddCSSClass("suggested-action")

	header := adw.NewHeaderBar()
	header.SetTitleWidget(titleWidget)
	headerActions := gtk.NewBox(gtk.OrientationHorizontal, 6)
	headerActions.Append(reprobe)
	headerActions.Append(continueBtn)
	header.PackEnd(headerActions)
	pageBox.Append(header)

	progressBar := gtk.NewProgressBar()
	progressBar.SetHExpand(true)
	progressBar.SetMarginStart(6)
	progressBar.SetMarginEnd(6)
	progressBar.Pulse()

	actionBar := gtk.NewActionBar()
	actionBar.SetCenterWidget(progressBar)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)

	prefPage := adw.NewPreferencesPage()
	scroll.SetChild(prefPage)

	toolbar := adw.NewToolbarView()
	toolbar.SetContent(scroll)
	toolbar.AddBottomBar(actionBar)
	pageBox.Append(toolbar)

	results := adw.NewPreferencesGroup()
	prefPage.Add(results)

	var pulseStop context.CancelFunc
	startPulse := func() context.CancelFunc {
		ctx, cancel := context.WithCancel(w.ctx)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(80 * time.Millisecond):
					glib.IdleAdd(func() { progressBar.Pulse() })
				}
			}
		}()
		return cancel
	}

	// agentNodes returns only the agent nodes from the current draft.
	agentNodes := func() []core.Node {
		var out []core.Node
		for _, n := range w.draft.Value().Nodes {
			if n.Role == core.RoleAgent {
				out = append(out, n)
			}
		}
		return out
	}

	startProbe := func() {
		reprobe.SetSensitive(false)
		titleWidget.SetSubtitle("Inspecting new node…")
		actionBar.SetVisible(true)
		if pulseStop != nil {
			pulseStop()
		}
		pulseStop = startPulse()
		prefPage.Remove(results)
		results = adw.NewPreferencesGroup()
		prefPage.Add(results)

		draft := w.draft.Value()
		if draft.Probes == nil {
			draft.Probes = map[string]*core.NodeProbe{}
		}
		nodes := agentNodes()
		for _, n := range nodes {
			delete(draft.Probes, n.ID)
		}
		w.draft.Pub(draft)

		rows := map[string]*probeRowState{}
		for _, n := range nodes {
			row := probePendingRow(n)
			rows[n.ID] = row
			results.Add(row.row)
		}
		go w.runAddNodeProbes(titleWidget, actionBar, pulseStop, rows, nodes, func() {
			reprobe.SetSensitive(true)
			pulseStop = nil
		})
	}
	reprobe.ConnectClicked(startProbe)

	continueBtn.ConnectClicked(func() {
		d := w.draft.Value()
		for _, n := range agentNodes() {
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
		w.push(w.addNodePlanPage())
	})

	startProbe()
	return navPage
}

// runAddNodeProbes runs SSH probes for a specific list of nodes (agents only).
func (w *Wizard) runAddNodeProbes(titleWidget *adw.WindowTitle, actionBar *gtk.ActionBar, pulseStop context.CancelFunc, rows map[string]*probeRowState, nodes []core.Node, done func()) {
	type result struct {
		node   core.Node
		probe  *core.NodeProbe
		client *core.Client
		err    error
	}
	results := make(chan result, len(nodes))

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

	for _, n := range nodes {
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

	for i := 0; i < len(nodes); i++ {
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
		if pulseStop != nil {
			pulseStop()
		}
		actionBar.SetVisible(false)
		titleWidget.SetSubtitle("")
		if done != nil {
			done()
		}
	})
}

// addNodePlanPage builds a plan via BuildAddNodePlan and shows it for review.
func (w *Wizard) addNodePlanPage() *adw.NavigationPage {
	d := w.draft.Value()
	plan, err := core.BuildAddNodePlan(d)
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

	return w.pageShell("Plan & Review", "Add Node", scroll, func() {
		w.push(w.applyPage())
	})
}
