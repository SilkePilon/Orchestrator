package ui

import (
	"context"
	"fmt"
	"sort"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/widget"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

type CostWasteView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	refresh *gtk.Button
	status  *gtk.Label
	results *gtk.Box
}

type costWasteOverview struct {
	Pods                  int
	Containers            int
	MissingCPURequests    int
	MissingMemoryRequests int
	MissingLimits         int
	IdleCPURequestMilli   int64
	IdleMemoryRequest     int64
	Findings              []costWasteFinding
}

type costWasteFinding struct {
	Title    string
	Detail   string
	Severity int
}

func NewCostWasteView(ctx context.Context, state *common.ClusterState) *CostWasteView {
	view := &CostWasteView{
		ToolbarView:  adw.NewToolbarView(),
		ClusterState: state,
		ctx:          ctx,
	}
	view.AddCSSClass("view")
	view.SetTopBarStyle(adw.ToolbarRaised)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("Cost", state.ClusterPreferences.Value().Name))

	view.refresh = gtk.NewButtonFromIconName("view-refresh-symbolic")
	view.refresh.AddCSSClass("flat")
	view.refresh.SetTooltipText("Refresh cost and waste recommendations")
	view.refresh.ConnectClicked(view.refreshCostWaste)
	header.PackEnd(view.refresh)
	view.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	view.SetContent(scroll)

	group := adw.NewPreferencesGroup()
	group.SetTitle("Cost / Waste Recommendations")
	group.SetDescription("Request, limit, and metrics-based hints for wasted or hard-to-price workloads.")
	page.Add(group)

	view.status = gtk.NewLabel("Loading recommendations...")
	view.status.SetHAlign(gtk.AlignStart)
	view.status.AddCSSClass("dim-label")
	group.Add(view.status)

	view.results = gtk.NewBox(gtk.OrientationVertical, 12)
	group.Add(view.results)

	view.refreshCostWaste()
	return view
}

func (v *CostWasteView) refreshCostWaste() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing recommendations...")
	clearBox(v.results)

	go func() {
		overview, err := v.collectCostWaste()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Could not load recommendations")
				widget.ShowErrorDialog(v.ctx, "Could not load cost recommendations", err)
				return
			}
			v.status.SetText("Recommendations loaded")
			v.renderCostWaste(overview)
		})
	}()
}

func (v *CostWasteView) collectCostWaste() (*costWasteOverview, error) {
	var pods corev1.PodList
	if err := v.List(v.ctx, &pods); err != nil {
		return nil, err
	}

	overview := &costWasteOverview{Pods: len(pods.Items)}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		metrics := v.Metrics.Pod(types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
		if pod.Status.QOSClass == corev1.PodQOSBestEffort {
			overview.Findings = append(overview.Findings, costWasteFinding{
				Title:    pod.Namespace + "/" + pod.Name,
				Detail:   "BestEffort pod has no requests, so scheduling and cost attribution are unreliable.",
				Severity: 70,
			})
		}

		for _, container := range pod.Spec.Containers {
			overview.Containers++
			name := pod.Namespace + "/" + pod.Name + " · " + container.Name
			cpuRequest := container.Resources.Requests.Cpu()
			memoryRequest := container.Resources.Requests.Memory()
			if cpuRequest == nil || cpuRequest.IsZero() {
				overview.MissingCPURequests++
				overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: "Missing CPU request.", Severity: 60})
			}
			if memoryRequest == nil || memoryRequest.IsZero() {
				overview.MissingMemoryRequests++
				overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: "Missing memory request.", Severity: 60})
			}
			if container.Resources.Limits.Cpu().IsZero() && container.Resources.Limits.Memory().IsZero() {
				overview.MissingLimits++
			}

			if metrics == nil {
				continue
			}
			for _, containerMetrics := range metrics.Containers {
				if containerMetrics.Name != container.Name {
					continue
				}
				if cpuRequest != nil && !cpuRequest.IsZero() {
					requestMilli := cpuRequest.MilliValue()
					usageMilli := containerMetrics.Usage.Cpu().MilliValue()
					if requestMilli >= 100 && usageMilli*5 < requestMilli {
						overview.IdleCPURequestMilli += requestMilli - usageMilli
						overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: fmt.Sprintf("CPU request %dm, current usage %dm.", requestMilli, usageMilli), Severity: 45})
					}
				}
				if memoryRequest != nil && !memoryRequest.IsZero() {
					requestBytes := memoryRequest.Value()
					usageBytes := containerMetrics.Usage.Memory().Value()
					if requestBytes >= 128*1024*1024 && usageBytes*3 < requestBytes {
						overview.IdleMemoryRequest += requestBytes - usageBytes
						overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: fmt.Sprintf("Memory request %s, current usage %s.", bytesQuantity(requestBytes), bytesQuantity(usageBytes)), Severity: 45})
					}
				}
			}
		}
	}
	sort.Slice(overview.Findings, func(i, j int) bool {
		if overview.Findings[i].Severity == overview.Findings[j].Severity {
			return overview.Findings[i].Title < overview.Findings[j].Title
		}
		return overview.Findings[i].Severity > overview.Findings[j].Severity
	})
	return overview, nil
}

func (v *CostWasteView) renderCostWaste(overview *costWasteOverview) {
	clearBox(v.results)

	summary := benchmarkCard()
	summary.Append(sectionLabel("Summary"))
	summary.Append(textRow("Pods", fmt.Sprintf("%d", overview.Pods)))
	summary.Append(textRow("Containers", fmt.Sprintf("%d", overview.Containers)))
	summary.Append(textRow("Missing CPU requests", fmt.Sprintf("%d", overview.MissingCPURequests)))
	summary.Append(textRow("Missing memory requests", fmt.Sprintf("%d", overview.MissingMemoryRequests)))
	summary.Append(textRow("Missing CPU and memory limits", fmt.Sprintf("%d", overview.MissingLimits)))
	summary.Append(textRow("Potential idle CPU request", fmt.Sprintf("%dm", overview.IdleCPURequestMilli)))
	summary.Append(textRow("Potential idle memory request", bytesQuantity(overview.IdleMemoryRequest)))
	v.results.Append(summary)

	findings := benchmarkCard()
	findings.Append(sectionLabel("Recommendations"))
	if len(overview.Findings) == 0 {
		findings.Append(textRow("No recommendations", "Requests and current metrics do not show obvious waste."))
	} else {
		limit := min(len(overview.Findings), 80)
		for i := 0; i < limit; i++ {
			finding := overview.Findings[i]
			findings.Append(textRow(finding.Title, finding.Detail))
		}
		if len(overview.Findings) > limit {
			findings.Append(textRow("More", fmt.Sprintf("%d additional recommendations", len(overview.Findings)-limit)))
		}
	}
	v.results.Append(findings)
}

func bytesQuantity(value int64) string {
	quantity := resource.NewQuantity(value, resource.BinarySI)
	return quantity.String()
}
