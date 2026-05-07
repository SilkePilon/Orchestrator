package ui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/getseabird/seabird/internal/ui/common"
	"github.com/getseabird/seabird/widget"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

type ProblemTimelineView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	refresh *gtk.Button
	status  *gtk.Label
	results *gtk.Box
}

type timelineEntry struct {
	When     time.Time
	Severity string
	Title    string
	Text     string
}

func NewProblemTimelineView(ctx context.Context, state *common.ClusterState) *ProblemTimelineView {
	view := &ProblemTimelineView{
		ToolbarView:  adw.NewToolbarView(),
		ClusterState: state,
		ctx:          ctx,
	}
	view.AddCSSClass("view")
	view.SetTopBarStyle(adw.ToolbarRaised)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("Timeline", state.ClusterPreferences.Value().Name))
	view.refresh = gtk.NewButtonFromIconName("view-refresh-symbolic")
	view.refresh.AddCSSClass("flat")
	view.refresh.SetTooltipText("Refresh problem timeline")
	view.refresh.ConnectClicked(view.refreshTimeline)
	header.PackEnd(view.refresh)
	view.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	view.SetContent(scroll)

	group := adw.NewPreferencesGroup()
	group.SetTitle("Problem Timeline")
	group.SetDescription("Recent Kubernetes events, pod restarts, waiting containers, and rollout health changes in one chronological view.")
	page.Add(group)

	view.status = gtk.NewLabel("Loading problem timeline...")
	view.status.SetHAlign(gtk.AlignStart)
	view.status.AddCSSClass("dim-label")
	group.Add(view.status)

	view.results = gtk.NewBox(gtk.OrientationVertical, 12)
	group.Add(view.results)

	view.refreshTimeline()
	return view
}

func (v *ProblemTimelineView) refreshTimeline() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing problem timeline...")
	clearBox(v.results)

	go func() {
		entries, err := v.collectProblemTimeline()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Could not refresh problem timeline.")
				widget.ShowErrorDialog(v.ctx, "Problem timeline failed", err)
				return
			}
			v.status.SetText(fmt.Sprintf("Updated %s", time.Now().Format("15:04:05")))
			v.renderTimeline(entries)
		})
	}()
}

func (v *ProblemTimelineView) collectProblemTimeline() ([]timelineEntry, error) {
	ctx, cancel := context.WithTimeout(v.ctx, 30*time.Second)
	defer cancel()

	var entries []timelineEntry
	var events corev1.EventList
	if err := v.Cluster.List(ctx, &events); err != nil {
		return nil, err
	}
	for _, event := range events.Items {
		severity := "Info"
		if event.Type == corev1.EventTypeWarning {
			severity = "Warning"
		}
		entries = append(entries, timelineEntry{
			When:     eventTime(event),
			Severity: severity,
			Title:    fmt.Sprintf("%s %s", event.InvolvedObject.Kind, namespacedName(event.Namespace, event.InvolvedObject.Name)),
			Text:     fmt.Sprintf("%s: %s", event.Reason, event.Message),
		})
	}

	var pods corev1.PodList
	if err := v.Cluster.List(ctx, &pods); err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		restarts := podRestartCount(pod)
		if restarts > 0 {
			entries = append(entries, timelineEntry{
				When:     pod.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(pod.Namespace, pod.Name),
				Text:     fmt.Sprintf("%d container restarts observed", restarts),
			})
		}
		if finding, ok := podHealthFinding(pod, restarts); ok {
			entries = append(entries, timelineEntry{
				When:     pod.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    finding.Title,
				Text:     finding.Text,
			})
		}
	}

	if err := v.appendRolloutTimeline(ctx, &entries); err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].When.After(entries[j].When) })
	if len(entries) > 80 {
		entries = entries[:80]
	}
	return entries, nil
}

func (v *ProblemTimelineView) appendRolloutTimeline(ctx context.Context, entries *[]timelineEntry) error {
	var deployments appsv1.DeploymentList
	if err := v.Cluster.List(ctx, &deployments); err != nil {
		return err
	}
	for _, deployment := range deployments.Items {
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		if deployment.Status.AvailableReplicas < desired {
			*entries = append(*entries, timelineEntry{
				When:     deployment.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(deployment.Namespace, deployment.Name),
				Text:     fmt.Sprintf("Deployment availability is %d/%d", deployment.Status.AvailableReplicas, desired),
			})
		}
	}

	var statefulSets appsv1.StatefulSetList
	if err := v.Cluster.List(ctx, &statefulSets); err != nil {
		return err
	}
	for _, statefulSet := range statefulSets.Items {
		desired := int32(1)
		if statefulSet.Spec.Replicas != nil {
			desired = *statefulSet.Spec.Replicas
		}
		if statefulSet.Status.ReadyReplicas < desired {
			*entries = append(*entries, timelineEntry{
				When:     statefulSet.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(statefulSet.Namespace, statefulSet.Name),
				Text:     fmt.Sprintf("StatefulSet readiness is %d/%d", statefulSet.Status.ReadyReplicas, desired),
			})
		}
	}

	var daemonSets appsv1.DaemonSetList
	if err := v.Cluster.List(ctx, &daemonSets); err != nil {
		return err
	}
	for _, daemonSet := range daemonSets.Items {
		desired := daemonSet.Status.DesiredNumberScheduled
		if daemonSet.Status.NumberReady < desired {
			*entries = append(*entries, timelineEntry{
				When:     daemonSet.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(daemonSet.Namespace, daemonSet.Name),
				Text:     fmt.Sprintf("DaemonSet readiness is %d/%d", daemonSet.Status.NumberReady, desired),
			})
		}
	}
	return nil
}

func (v *ProblemTimelineView) renderTimeline(entries []timelineEntry) {
	clearBox(v.results)
	card := benchmarkCard()
	card.Append(sectionLabel("Latest Signals"))
	if len(entries) == 0 {
		card.Append(textRow("Status", "No recent problems found."))
		v.results.Append(card)
		return
	}
	for _, entry := range entries {
		title := fmt.Sprintf("%s · %s · %s", entry.When.Format("Jan 2 15:04"), entry.Severity, entry.Title)
		card.Append(textRow(title, entry.Text))
	}
	v.results.Append(card)
}
