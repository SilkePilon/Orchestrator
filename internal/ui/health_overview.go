package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/getseabird/seabird/internal/ui/common"
	"github.com/getseabird/seabird/widget"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

type HealthOverviewView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	refresh *gtk.Button
	status  *gtk.Label
	results *gtk.Box
}

type clusterHealthOverview struct {
	CollectedAt         time.Time
	NodesTotal          int
	NodesReady          int
	PodsTotal           int
	PodsRunning         int
	PodsPending         int
	PodsFailed          int
	PodWarnings         []healthFinding
	TotalRestarts       int32
	DeploymentsTotal    int
	DeploymentsHealthy  int
	StatefulSetsTotal   int
	StatefulSetsHealthy int
	DaemonSetsTotal     int
	DaemonSetsHealthy   int
	WorkloadWarnings    []healthFinding
	RecentWarningEvents []healthFinding
}

type healthFinding struct {
	Title string
	Text  string
}

func NewHealthOverviewView(ctx context.Context, state *common.ClusterState) *HealthOverviewView {
	view := &HealthOverviewView{
		ToolbarView:  adw.NewToolbarView(),
		ClusterState: state,
		ctx:          ctx,
	}
	view.AddCSSClass("view")
	view.SetTopBarStyle(adw.ToolbarRaised)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("Health", state.ClusterPreferences.Value().Name))

	view.refresh = gtk.NewButtonFromIconName("view-refresh-symbolic")
	view.refresh.AddCSSClass("flat")
	view.refresh.SetTooltipText("Refresh cluster health")
	view.refresh.ConnectClicked(view.refreshHealth)
	header.PackEnd(view.refresh)
	view.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	view.SetContent(scroll)

	group := adw.NewPreferencesGroup()
	group.SetTitle("Cluster Health")
	group.SetDescription("A quick operational overview of nodes, workloads, pod restarts, and recent warning events.")
	page.Add(group)

	view.status = gtk.NewLabel("Loading cluster health...")
	view.status.SetHAlign(gtk.AlignStart)
	view.status.AddCSSClass("dim-label")
	group.Add(view.status)

	view.results = gtk.NewBox(gtk.OrientationVertical, 12)
	group.Add(view.results)

	view.refreshHealth()
	return view
}

func (v *HealthOverviewView) refreshHealth() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing cluster health...")
	clearBox(v.results)

	go func() {
		overview, err := v.collectHealthOverview()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Could not refresh cluster health.")
				widget.ShowErrorDialog(v.ctx, "Cluster health failed", err)
				return
			}
			v.status.SetText(fmt.Sprintf("Updated %s", overview.CollectedAt.Format("15:04:05")))
			v.renderHealthOverview(overview)
		})
	}()
}

func (v *HealthOverviewView) collectHealthOverview() (*clusterHealthOverview, error) {
	ctx, cancel := context.WithTimeout(v.ctx, 30*time.Second)
	defer cancel()
	overview := &clusterHealthOverview{CollectedAt: time.Now()}

	var nodes corev1.NodeList
	if err := v.Cluster.List(ctx, &nodes); err != nil {
		return nil, err
	}
	overview.NodesTotal = len(nodes.Items)
	for _, node := range nodes.Items {
		if nodeReady(node) {
			overview.NodesReady++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: node.Name,
			Text:  "Node is not Ready",
		})
	}

	var pods corev1.PodList
	if err := v.Cluster.List(ctx, &pods); err != nil {
		return nil, err
	}
	overview.PodsTotal = len(pods.Items)
	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded:
			overview.PodsRunning++
		case corev1.PodPending:
			overview.PodsPending++
		case corev1.PodFailed:
			overview.PodsFailed++
		}
		restarts := podRestartCount(pod)
		overview.TotalRestarts += restarts
		if finding, ok := podHealthFinding(pod, restarts); ok {
			overview.PodWarnings = append(overview.PodWarnings, finding)
		}
	}

	if err := v.collectWorkloadHealth(ctx, overview); err != nil {
		return nil, err
	}
	if err := v.collectWarningEvents(ctx, overview); err != nil {
		return nil, err
	}

	return overview, nil
}

func (v *HealthOverviewView) collectWorkloadHealth(ctx context.Context, overview *clusterHealthOverview) error {
	var deployments appsv1.DeploymentList
	if err := v.Cluster.List(ctx, &deployments); err != nil {
		return err
	}
	overview.DeploymentsTotal = len(deployments.Items)
	for _, deployment := range deployments.Items {
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		if deployment.Status.AvailableReplicas >= desired {
			overview.DeploymentsHealthy++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: namespacedName(deployment.Namespace, deployment.Name),
			Text:  fmt.Sprintf("Deployment has %d/%d available replicas", deployment.Status.AvailableReplicas, desired),
		})
	}

	var statefulSets appsv1.StatefulSetList
	if err := v.Cluster.List(ctx, &statefulSets); err != nil {
		return err
	}
	overview.StatefulSetsTotal = len(statefulSets.Items)
	for _, statefulSet := range statefulSets.Items {
		desired := int32(1)
		if statefulSet.Spec.Replicas != nil {
			desired = *statefulSet.Spec.Replicas
		}
		if statefulSet.Status.ReadyReplicas >= desired {
			overview.StatefulSetsHealthy++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: namespacedName(statefulSet.Namespace, statefulSet.Name),
			Text:  fmt.Sprintf("StatefulSet has %d/%d ready replicas", statefulSet.Status.ReadyReplicas, desired),
		})
	}

	var daemonSets appsv1.DaemonSetList
	if err := v.Cluster.List(ctx, &daemonSets); err != nil {
		return err
	}
	overview.DaemonSetsTotal = len(daemonSets.Items)
	for _, daemonSet := range daemonSets.Items {
		desired := daemonSet.Status.DesiredNumberScheduled
		if daemonSet.Status.NumberReady >= desired {
			overview.DaemonSetsHealthy++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: namespacedName(daemonSet.Namespace, daemonSet.Name),
			Text:  fmt.Sprintf("DaemonSet has %d/%d ready pods", daemonSet.Status.NumberReady, desired),
		})
	}

	return nil
}

func (v *HealthOverviewView) collectWarningEvents(ctx context.Context, overview *clusterHealthOverview) error {
	var events corev1.EventList
	if err := v.Cluster.List(ctx, &events); err != nil {
		return err
	}
	sort.Slice(events.Items, func(i, j int) bool {
		return eventTime(events.Items[i]).After(eventTime(events.Items[j]))
	})
	for _, event := range events.Items {
		if event.Type != corev1.EventTypeWarning {
			continue
		}
		overview.RecentWarningEvents = append(overview.RecentWarningEvents, healthFinding{
			Title: fmt.Sprintf("%s %s", event.InvolvedObject.Kind, namespacedName(event.Namespace, event.InvolvedObject.Name)),
			Text:  strings.TrimSpace(fmt.Sprintf("%s: %s", event.Reason, event.Message)),
		})
		if len(overview.RecentWarningEvents) >= 8 {
			break
		}
	}
	return nil
}

func (v *HealthOverviewView) renderHealthOverview(overview *clusterHealthOverview) {
	clearBox(v.results)

	summary := benchmarkCard()
	summary.Append(sectionLabel("Summary"))
	summary.Append(barRow("Nodes ready", fmt.Sprintf("%d / %d", overview.NodesReady, overview.NodesTotal), float64(overview.NodesReady), float64(max(overview.NodesTotal, 1))))
	summary.Append(barRow("Pods running", fmt.Sprintf("%d / %d", overview.PodsRunning, overview.PodsTotal), float64(overview.PodsRunning), float64(max(overview.PodsTotal, 1))))
	summary.Append(textRow("Pending pods", fmt.Sprintf("%d", overview.PodsPending)))
	summary.Append(textRow("Failed pods", fmt.Sprintf("%d", overview.PodsFailed)))
	summary.Append(textRow("Container restarts", fmt.Sprintf("%d", overview.TotalRestarts)))
	v.results.Append(summary)

	workloads := benchmarkCard()
	workloads.Append(sectionLabel("Workloads"))
	workloads.Append(barRow("Deployments", fmt.Sprintf("%d / %d healthy", overview.DeploymentsHealthy, overview.DeploymentsTotal), float64(overview.DeploymentsHealthy), float64(max(overview.DeploymentsTotal, 1))))
	workloads.Append(barRow("StatefulSets", fmt.Sprintf("%d / %d healthy", overview.StatefulSetsHealthy, overview.StatefulSetsTotal), float64(overview.StatefulSetsHealthy), float64(max(overview.StatefulSetsTotal, 1))))
	workloads.Append(barRow("DaemonSets", fmt.Sprintf("%d / %d healthy", overview.DaemonSetsHealthy, overview.DaemonSetsTotal), float64(overview.DaemonSetsHealthy), float64(max(overview.DaemonSetsTotal, 1))))
	appendFindings(workloads, overview.WorkloadWarnings, "No workload or node warnings found.")
	v.results.Append(workloads)

	pods := benchmarkCard()
	pods.Append(sectionLabel("Pod Issues"))
	appendFindings(pods, overview.PodWarnings, "No pod crash, waiting, or restart warnings found.")
	v.results.Append(pods)

	events := benchmarkCard()
	events.Append(sectionLabel("Recent Warning Events"))
	appendFindings(events, overview.RecentWarningEvents, "No recent warning events found.")
	v.results.Append(events)
}

func appendFindings(card *gtk.Box, findings []healthFinding, empty string) {
	if len(findings) == 0 {
		card.Append(textRow("Status", empty))
		return
	}
	for _, finding := range findings {
		card.Append(textRow(finding.Title, finding.Text))
	}
}

func podRestartCount(pod corev1.Pod) int32 {
	var restarts int32
	for _, status := range pod.Status.ContainerStatuses {
		restarts += status.RestartCount
	}
	return restarts
}

func podHealthFinding(pod corev1.Pod, restarts int32) (healthFinding, bool) {
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodPending {
		return healthFinding{Title: namespacedName(pod.Namespace, pod.Name), Text: fmt.Sprintf("Pod is %s", pod.Status.Phase)}, true
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			return healthFinding{
				Title: namespacedName(pod.Namespace, pod.Name),
				Text:  fmt.Sprintf("%s is waiting: %s", status.Name, status.State.Waiting.Reason),
			}, true
		}
	}
	if restarts > 0 {
		return healthFinding{Title: namespacedName(pod.Namespace, pod.Name), Text: fmt.Sprintf("%d container restarts", restarts)}, true
	}
	return healthFinding{}, false
}

func eventTime(event corev1.Event) time.Time {
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time
	}
	return event.CreationTimestamp.Time
}

func namespacedName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}
