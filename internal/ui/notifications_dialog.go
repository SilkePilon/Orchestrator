package ui

import (
	"context"
	"strings"

	"github.com/SilkePilon/Orchestrator/api"
	gitopsbe "github.com/SilkePilon/Orchestrator/internal/gitops"
	"github.com/SilkePilon/Orchestrator/internal/pubsub"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewNotificationsDialog builds and presents a notifications preferences dialog
// for the given cluster. GitOps sections are shown only when the respective
// engine is detected on the cluster (detection runs in the background).
func NewNotificationsDialog(ctx context.Context, clusterPrefs pubsub.Property[api.ClusterPreferences], c client.Client) *adw.Dialog {
	dialog := adw.NewDialog()
	dialog.SetTitle("Notifications")
	dialog.SetContentWidth(560)
	dialog.SetContentHeight(620)

	box := gtk.NewBox(gtk.OrientationVertical, 0)

	header := adw.NewHeaderBar()
	box.Append(header)

	scrolled := gtk.NewScrolledWindow()
	scrolled.SetHExpand(true)
	scrolled.SetVExpand(true)
	scrolled.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)

	page := adw.NewPreferencesPage()
	scrolled.SetChild(page)
	box.Append(scrolled)

	dialog.SetChild(box)

	// update reads the current prefs, applies fn, and publishes the result.
	update := func(fn func(*api.NotificationPreferences)) {
		cp := clusterPrefs.Value()
		fn(&cp.Notifications)
		clusterPrefs.Pub(cp)
	}

	prefs := clusterPrefs.Value().Notifications

	// makeSwitchRow creates an adw.SwitchRow already added to g, with a live-save handler.
	makeSwitchRow := func(g *adw.PreferencesGroup, title, subtitle string, active bool, save func(bool)) *adw.SwitchRow {
		row := adw.NewSwitchRow()
		row.SetTitle(title)
		if subtitle != "" {
			row.SetSubtitle(subtitle)
		}
		row.SetActive(active)
		row.Connect("notify::active", func() { save(row.Active()) })
		g.Add(row)
		return row
	}

	// ── Master switch ──────────────────────────────────────────────────────
	masterGroup := adw.NewPreferencesGroup()
	masterSwitch := makeSwitchRow(masterGroup, "Enable notifications",
		"Receive desktop notifications for events on this cluster",
		prefs.Enabled, func(v bool) { update(func(n *api.NotificationPreferences) { n.Enabled = v }) })
	page.Add(masterGroup)

	// ── Workloads ──────────────────────────────────────────────────────────
	workloadsGroup := adw.NewPreferencesGroup()
	workloadsGroup.SetTitle("Workloads")
	podRow := makeSwitchRow(workloadsGroup, "Pod failures",
		"OOMKilled, ImagePullBackOff, Error and similar states",
		prefs.PodFailures, func(v bool) { update(func(n *api.NotificationPreferences) { n.PodFailures = v }) })
	crashRow := makeSwitchRow(workloadsGroup, "Crash loop backoffs",
		"Pods stuck in CrashLoopBackOff",
		prefs.PodCrashLoops, func(v bool) { update(func(n *api.NotificationPreferences) { n.PodCrashLoops = v }) })
	restartRow := makeSwitchRow(workloadsGroup, "Container restarts",
		"Any container restart count increases",
		prefs.ContainerRestarts, func(v bool) { update(func(n *api.NotificationPreferences) { n.ContainerRestarts = v }) })
	newPodRow := makeSwitchRow(workloadsGroup, "New pod scheduled",
		"A pod is scheduled onto the cluster",
		prefs.NewPod, func(v bool) { update(func(n *api.NotificationPreferences) { n.NewPod = v }) })
	deployRow := makeSwitchRow(workloadsGroup, "Deployment rollouts",
		"Deployments with unavailable replicas",
		prefs.DeploymentRollouts, func(v bool) { update(func(n *api.NotificationPreferences) { n.DeploymentRollouts = v }) })
	newDeployRow := makeSwitchRow(workloadsGroup, "New workload created",
		"A new Deployment, StatefulSet or DaemonSet is created",
		prefs.NewDeployment, func(v bool) { update(func(n *api.NotificationPreferences) { n.NewDeployment = v }) })
	jobRow := makeSwitchRow(workloadsGroup, "Job failures",
		"Batch jobs that complete with a failure",
		prefs.JobFailures, func(v bool) { update(func(n *api.NotificationPreferences) { n.JobFailures = v }) })
	jobDoneRow := makeSwitchRow(workloadsGroup, "Job completed",
		"Batch jobs that finish successfully",
		prefs.JobCompleted, func(v bool) { update(func(n *api.NotificationPreferences) { n.JobCompleted = v }) })
	page.Add(workloadsGroup)

	// ── Cluster health ─────────────────────────────────────────────────────
	clusterGroup := adw.NewPreferencesGroup()
	clusterGroup.SetTitle("Cluster")
	nodeRow := makeSwitchRow(clusterGroup, "Node becomes unavailable",
		"Node transitions to NotReady",
		prefs.NodeNotReady, func(v bool) { update(func(n *api.NotificationPreferences) { n.NodeNotReady = v }) })
	newNodeRow := makeSwitchRow(clusterGroup, "Node joined",
		"A new node registers with the cluster",
		prefs.NewNode, func(v bool) { update(func(n *api.NotificationPreferences) { n.NewNode = v }) })
	pvcRow := makeSwitchRow(clusterGroup, "PVC stuck pending",
		"Persistent volume claims that cannot be bound",
		prefs.PVCPending, func(v bool) { update(func(n *api.NotificationPreferences) { n.PVCPending = v }) })
	configRow := makeSwitchRow(clusterGroup, "ConfigMap or Secret updated",
		"Any ConfigMap or Secret is modified",
		prefs.ConfigChange, func(v bool) { update(func(n *api.NotificationPreferences) { n.ConfigChange = v }) })
	nsRow2 := makeSwitchRow(clusterGroup, "New namespace created",
		"A namespace is added to the cluster",
		prefs.NewNamespace, func(v bool) { update(func(n *api.NotificationPreferences) { n.NewNamespace = v }) })
	ingressRow := makeSwitchRow(clusterGroup, "New Ingress created",
		"An Ingress resource is added",
		prefs.NewIngress, func(v bool) { update(func(n *api.NotificationPreferences) { n.NewIngress = v }) })
	page.Add(clusterGroup)

	// ── Argo CD (hidden until detected) ────────────────────────────────────
	argoGroup := adw.NewPreferencesGroup()
	argoGroup.SetTitle("Argo CD")
	argoGroup.SetDescription("Notifications for Argo CD applications on this cluster")
	argoGroup.SetVisible(false)
	argoSyncRow := makeSwitchRow(argoGroup, "Application out of sync",
		"Application drifts from the desired Git state",
		prefs.ArgoAppOutOfSync, func(v bool) { update(func(n *api.NotificationPreferences) { n.ArgoAppOutOfSync = v }) })
	argoDegRow := makeSwitchRow(argoGroup, "Application degraded",
		"Application health drops to Degraded",
		prefs.ArgoAppDegraded, func(v bool) { update(func(n *api.NotificationPreferences) { n.ArgoAppDegraded = v }) })
	page.Add(argoGroup)

	// ── Flux CD (hidden until detected) ────────────────────────────────────
	fluxGroup := adw.NewPreferencesGroup()
	fluxGroup.SetTitle("Flux CD")
	fluxGroup.SetDescription("Notifications for Flux CD resources on this cluster")
	fluxGroup.SetVisible(false)
	fluxKustRow := makeSwitchRow(fluxGroup, "Kustomization failed",
		"A Kustomization resource fails to reconcile",
		prefs.FluxKustomizationFailed, func(v bool) { update(func(n *api.NotificationPreferences) { n.FluxKustomizationFailed = v }) })
	fluxHelmRow := makeSwitchRow(fluxGroup, "HelmRelease failed",
		"A HelmRelease resource fails to reconcile",
		prefs.FluxHelmReleaseFailed, func(v bool) { update(func(n *api.NotificationPreferences) { n.FluxHelmReleaseFailed = v }) })
	fluxSrcRow := makeSwitchRow(fluxGroup, "Source not ready",
		"GitRepository or HelmRepository becomes unavailable",
		prefs.FluxSourceNotReady, func(v bool) { update(func(n *api.NotificationPreferences) { n.FluxSourceNotReady = v }) })
	page.Add(fluxGroup)

	// ── Advanced ───────────────────────────────────────────────────────────
	advGroup := adw.NewPreferencesGroup()
	advGroup.SetTitle("Advanced")

	cooldownLabels := []string{"Never", "1 minute", "5 minutes", "15 minutes", "30 minutes"}
	cooldownValues := []int{0, 60, 300, 900, 1800}
	cooldownRow := adw.NewComboRow()
	cooldownRow.SetTitle("Cooldown period")
	cooldownRow.SetSubtitle("Minimum time between repeated notifications for the same resource")
	cooldownRow.SetModel(gtk.NewStringList(cooldownLabels))
	selectedIdx := 2
	for i, v := range cooldownValues {
		if v == prefs.CooldownSeconds {
			selectedIdx = i
			break
		}
	}
	cooldownRow.SetSelected(uint(selectedIdx))
	cooldownRow.Connect("notify::selected", func() {
		idx := int(cooldownRow.Selected())
		if idx >= 0 && idx < len(cooldownValues) {
			update(func(n *api.NotificationPreferences) { n.CooldownSeconds = cooldownValues[idx] })
		}
	})
	advGroup.Add(cooldownRow)

	nsRow := adw.NewEntryRow()
	nsRow.SetTitle("Excluded namespaces")
	nsRow.SetText(strings.Join(prefs.ExcludedNamespaces, ", "))
	nsRow.Connect("changed", func() {
		update(func(n *api.NotificationPreferences) { n.ExcludedNamespaces = splitTrimmed(nsRow.Text()) })
	})
	advGroup.Add(nsRow)

	resRow := adw.NewEntryRow()
	resRow.SetTitle("Excluded resources (namespace/name)")
	resRow.SetText(strings.Join(prefs.ExcludedResources, ", "))
	resRow.Connect("changed", func() {
		update(func(n *api.NotificationPreferences) { n.ExcludedResources = splitTrimmed(resRow.Text()) })
	})
	advGroup.Add(resRow)

	page.Add(advGroup)

	// ── Master-switch sensitivity ──────────────────────────────────────────
	subGroups := []*adw.PreferencesGroup{workloadsGroup, clusterGroup, argoGroup, fluxGroup, advGroup}
	subRows := []interface{ SetSensitive(bool) }{
		podRow, crashRow, restartRow, newPodRow, deployRow, newDeployRow, jobRow, jobDoneRow,
		nodeRow, newNodeRow, pvcRow, configRow, nsRow2, ingressRow,
		argoSyncRow, argoDegRow,
		fluxKustRow, fluxHelmRow, fluxSrcRow,
		cooldownRow, nsRow, resRow,
	}
	setSensitivity := func(active bool) {
		for _, g := range subGroups {
			g.SetSensitive(active)
		}
		for _, r := range subRows {
			r.SetSensitive(active)
		}
	}
	masterSwitch.Connect("notify::active", func() { setSensitivity(masterSwitch.Active()) })
	setSensitivity(prefs.Enabled)

	// ── Async GitOps detection ─────────────────────────────────────────────
	go func() {
		det, err := gitopsbe.Detect(ctx, c)
		if err != nil {
			return
		}
		glib.IdleAdd(func() {
			if det.ArgoCD {
				argoGroup.SetVisible(true)
			}
			if det.Flux {
				fluxGroup.SetVisible(true)
			}
		})
	}()

	return dialog
}

// splitTrimmed splits a comma-separated string into trimmed, non-empty parts.
func splitTrimmed(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}
