// Package visualizer renders a compact, at-a-glance dashboard of the cluster:
// a summary strip on top followed by a horizontally-flowing grid of small
// node cards. Detail views are delegated to the shared bottom sheet.
package visualizer

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/zmwangx/debounce"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

var (
	nodesGVR       = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}
	podsGVR        = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	namespacesGVR  = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	servicesGVR    = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

type Visualizer struct {
	*adw.ToolbarView

	ctx       context.Context
	state     *common.ClusterState
	dialog    *adw.Dialog
	container *gtk.Box
	rebuild   func()
}

func NewVisualizer(ctx context.Context, state *common.ClusterState, dialog *adw.Dialog) *Visualizer {
	v := &Visualizer{
		ToolbarView: adw.NewToolbarView(),
		ctx:         ctx,
		state:       state,
		dialog:      dialog,
	}
	v.AddCSSClass("view")

	header := adw.NewHeaderBar()
	title := gtk.NewLabel("Cluster Visualizer")
	title.AddCSSClass("heading")
	header.SetTitleWidget(title)

	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.AddCSSClass("flat")
	refresh.SetTooltipText("Refresh")
	refresh.ConnectClicked(func() { v.render() })
	header.PackEnd(refresh)
	v.AddTopBar(header)

	v.container = gtk.NewBox(gtk.OrientationVertical, 18)
	v.container.SetMarginTop(18)
	v.container.SetMarginBottom(18)
	v.container.SetMarginStart(18)
	v.container.SetMarginEnd(18)

	scroll := gtk.NewScrolledWindow()
	scroll.SetHExpand(true)
	scroll.SetVExpand(true)
	scroll.SetChild(v.container)
	v.SetContent(scroll)

	debounced, _ := debounce.Debounce(func() {
		glib.IdleAdd(v.render)
	}, 250*time.Millisecond)
	v.rebuild = debounced

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { v.rebuild() },
		UpdateFunc: func(_, _ interface{}) { v.rebuild() },
		DeleteFunc: func(_ interface{}) { v.rebuild() },
	}
	for _, gvr := range []schema.GroupVersionResource{nodesGVR, podsGVR, namespacesGVR, servicesGVR, deploymentsGVR} {
		if err := state.Cluster.AddInformerEventHandler(ctx, gvr, handler); err != nil {
			klog.Warningf("visualizer: %s watch: %v", gvr.Resource, err)
		}
	}

	v.placeholder("Loading cluster topology…")
	v.rebuild()

	return v
}

func (v *Visualizer) clear() {
	for child := v.container.FirstChild(); child != nil; child = v.container.FirstChild() {
		v.container.Remove(child)
	}
}

func (v *Visualizer) placeholder(text string) {
	v.clear()
	status := adw.NewStatusPage()
	status.SetIconName("network-workgroup-symbolic")
	status.SetTitle(text)
	status.SetVExpand(true)
	v.container.Append(status)
}

func (v *Visualizer) render() {
	cluster := v.state.Cluster

	var nodes []*corev1.Node
	listInto(cluster, nodesGVR, func(o interface{}) {
		if n, ok := o.(*corev1.Node); ok {
			nodes = append(nodes, n)
		}
	})
	var pods []*corev1.Pod
	listInto(cluster, podsGVR, func(o interface{}) {
		if p, ok := o.(*corev1.Pod); ok {
			pods = append(pods, p)
		}
	})
	var namespaces []*corev1.Namespace
	listInto(cluster, namespacesGVR, func(o interface{}) {
		if n, ok := o.(*corev1.Namespace); ok {
			namespaces = append(namespaces, n)
		}
	})
	var services []*corev1.Service
	listInto(cluster, servicesGVR, func(o interface{}) {
		if s, ok := o.(*corev1.Service); ok {
			services = append(services, s)
		}
	})
	var deployments []*appsv1.Deployment
	listInto(cluster, deploymentsGVR, func(o interface{}) {
		if d, ok := o.(*appsv1.Deployment); ok {
			deployments = append(deployments, d)
		}
	})

	if len(nodes) == 0 && len(pods) == 0 {
		v.placeholder("Waiting for cluster data…")
		return
	}

	sort.SliceStable(nodes, func(i, j int) bool {
		ci, cj := isControlPlane(nodes[i]), isControlPlane(nodes[j])
		if ci != cj {
			return ci
		}
		return nodes[i].Name < nodes[j].Name
	})

	byNode := map[string][]*corev1.Pod{}
	for _, p := range pods {
		byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
	}

	v.clear()
	v.container.Append(v.makeSummary(nodes, pods, namespaces, services, deployments))
	v.container.Append(sectionHeader("Nodes", fmt.Sprintf("%d total", len(nodes))))
	v.container.Append(v.makeNodeGrid(nodes, byNode))

	if orphans := byNode[""]; len(orphans) > 0 {
		v.container.Append(sectionHeader("Unscheduled pods", fmt.Sprintf("%d", len(orphans))))
		v.container.Append(v.makeUnscheduledCard(orphans))
	}
}

func listInto(cluster *api.Cluster, gvr schema.GroupVersionResource, fn func(interface{})) {
	inf := cluster.GetInformer(gvr)
	if inf == nil {
		return
	}
	if err := cache.ListAll(inf.Informer().GetIndexer(), labels.Everything(), fn); err != nil {
		klog.Warningf("visualizer: list %s: %v", gvr.Resource, err)
	}
}

// ---------- Summary strip ----------

func (v *Visualizer) makeSummary(nodes []*corev1.Node, pods []*corev1.Pod, ns []*corev1.Namespace, svc []*corev1.Service, deps []*appsv1.Deployment) gtk.Widgetter {
	readyNodes := 0
	for _, n := range nodes {
		if r, _ := nodeReadiness(n); r {
			readyNodes++
		}
	}
	running, pending, failed, succeeded := 0, 0, 0, 0
	for _, p := range pods {
		switch p.Status.Phase {
		case corev1.PodRunning:
			running++
		case corev1.PodPending:
			pending++
		case corev1.PodFailed:
			failed++
		case corev1.PodSucceeded:
			succeeded++
		}
	}

	card := gtk.NewBox(gtk.OrientationVertical, 8)
	card.AddCSSClass("card")

	inner := gtk.NewBox(gtk.OrientationVertical, 8)
	inner.SetMarginTop(12)
	inner.SetMarginBottom(12)
	inner.SetMarginStart(14)
	inner.SetMarginEnd(14)

	clusterName := v.state.Cluster.ClusterPreferences.Value().Name
	if clusterName == "" {
		clusterName = "Cluster"
	}
	titleRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	title := gtk.NewLabel(clusterName)
	title.AddCSSClass("title-3")
	title.SetHAlign(gtk.AlignStart)
	title.SetEllipsize(pango.EllipsizeEnd)
	title.SetHExpand(true)
	titleRow.Append(title)
	if dist := v.state.Cluster.Distribution; dist != "" {
		titleRow.Append(pillLabel(dist, "accent", ""))
	}
	inner.Append(titleRow)

	chips := gtk.NewFlowBox()
	chips.SetSelectionMode(gtk.SelectionNone)
	chips.SetHomogeneous(false)
	chips.SetMaxChildrenPerLine(20)
	chips.SetMinChildrenPerLine(1)
	chips.SetColumnSpacing(6)
	chips.SetRowSpacing(6)
	chips.SetActivateOnSingleClick(false)
	chips.SetHAlign(gtk.AlignFill)
	chips.SetHExpand(true)

	addChip := func(w gtk.Widgetter) {
		child := gtk.NewFlowBoxChild()
		child.SetFocusable(false)
		child.SetChild(w)
		chips.Insert(child, -1)
	}

	addChip(metricChip("computer-symbolic", "Nodes", fmt.Sprintf("%d/%d", readyNodes, len(nodes)), readyNodesSemantic(readyNodes, len(nodes))))
	addChip(metricChip("application-x-executable-symbolic", "Pods", fmt.Sprintf("%d", len(pods)), ""))
	if running > 0 {
		addChip(metricChip("media-playback-start-symbolic", "Running", fmt.Sprintf("%d", running), "success"))
	}
	if pending > 0 {
		addChip(metricChip("content-loading-symbolic", "Pending", fmt.Sprintf("%d", pending), "warning"))
	}
	if failed > 0 {
		addChip(metricChip("dialog-error-symbolic", "Failed", fmt.Sprintf("%d", failed), "error"))
	}
	if succeeded > 0 {
		addChip(metricChip("emblem-default-symbolic", "Succeeded", fmt.Sprintf("%d", succeeded), "dim-label"))
	}
	addChip(metricChip("folder-symbolic", "Namespaces", fmt.Sprintf("%d", len(ns)), ""))
	addChip(metricChip("network-server-symbolic", "Services", fmt.Sprintf("%d", len(svc)), ""))
	addChip(metricChip("preferences-system-symbolic", "Deployments", fmt.Sprintf("%d", len(deps)), ""))

	inner.Append(chips)

	card.Append(inner)
	return card
}

func metricChip(icon, label, value, semantic string) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationHorizontal, 0)
	box.AddCSSClass("card")

	inner := gtk.NewBox(gtk.OrientationHorizontal, 6)
	inner.SetMarginTop(6)
	inner.SetMarginBottom(6)
	inner.SetMarginStart(10)
	inner.SetMarginEnd(10)

	if icon != "" {
		img := gtk.NewImageFromIconName(icon)
		img.AddCSSClass("dim-label")
		inner.Append(img)
	}

	val := gtk.NewLabel(value)
	val.AddCSSClass("heading")
	if semantic != "" {
		val.AddCSSClass(semantic)
	}
	inner.Append(val)

	l := gtk.NewLabel(label)
	l.AddCSSClass("caption")
	l.AddCSSClass("dim-label")
	inner.Append(l)

	box.Append(inner)
	return box
}

func readyNodesSemantic(ready, total int) string {
	switch {
	case total == 0:
		return "dim-label"
	case ready == total:
		return "success"
	case ready == 0:
		return "error"
	default:
		return "warning"
	}
}

// ---------- Node grid ----------

func sectionHeader(title, suffix string) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationHorizontal, 8)
	box.SetMarginTop(4)
	t := gtk.NewLabel(title)
	t.AddCSSClass("title-4")
	t.SetHAlign(gtk.AlignStart)
	box.Append(t)
	if suffix != "" {
		s := gtk.NewLabel(suffix)
		s.AddCSSClass("caption")
		s.AddCSSClass("dim-label")
		s.SetHAlign(gtk.AlignStart)
		s.SetVAlign(gtk.AlignCenter)
		box.Append(s)
	}
	return box
}

func (v *Visualizer) makeNodeGrid(nodes []*corev1.Node, byNode map[string][]*corev1.Pod) gtk.Widgetter {
	flow := gtk.NewFlowBox()
	flow.SetSelectionMode(gtk.SelectionNone)
	flow.SetHomogeneous(true)
	flow.SetMaxChildrenPerLine(6)
	flow.SetMinChildrenPerLine(1)
	flow.SetColumnSpacing(12)
	flow.SetRowSpacing(12)
	flow.SetActivateOnSingleClick(false)
	flow.SetHExpand(true)

	for _, node := range nodes {
		child := gtk.NewFlowBoxChild()
		child.SetFocusable(false)
		child.SetChild(v.makeNodeCard(node, byNode[node.Name]))
		flow.Insert(child, -1)
	}
	return flow
}

func (v *Visualizer) makeNodeCard(node *corev1.Node, pods []*corev1.Pod) gtk.Widgetter {
	card := gtk.NewBox(gtk.OrientationVertical, 6)
	card.AddCSSClass("card")
	card.AddCSSClass("activatable")
	card.SetSizeRequest(280, -1)

	header := gtk.NewBox(gtk.OrientationHorizontal, 6)
	header.SetMarginTop(10)
	header.SetMarginStart(12)
	header.SetMarginEnd(12)

	statusDot := gtk.NewLabel("●")
	if r, _ := nodeReadiness(node); r {
		statusDot.AddCSSClass("success")
	} else {
		statusDot.AddCSSClass("error")
	}
	header.Append(statusDot)

	name := gtk.NewLabel(node.Name)
	name.AddCSSClass("heading")
	name.SetHAlign(gtk.AlignStart)
	name.SetHExpand(true)
	name.SetEllipsize(pango.EllipsizeEnd)
	name.SetMaxWidthChars(20)
	header.Append(name)

	if isControlPlane(node) {
		header.Append(pillLabel("CP", "success", "Control Plane"))
	} else {
		header.Append(pillLabel("W", "accent", "Worker"))
	}
	card.Append(header)

	info := node.Status.NodeInfo
	parts := []string{}
	if info.KubeletVersion != "" {
		parts = append(parts, info.KubeletVersion)
	}
	if info.OperatingSystem != "" || info.Architecture != "" {
		parts = append(parts, fmt.Sprintf("%s/%s", info.OperatingSystem, info.Architecture))
	}
	if len(parts) > 0 {
		sub := gtk.NewLabel(joinDot(parts))
		sub.AddCSSClass("caption")
		sub.AddCSSClass("dim-label")
		sub.SetHAlign(gtk.AlignStart)
		sub.SetEllipsize(pango.EllipsizeEnd)
		sub.SetMarginStart(12)
		sub.SetMarginEnd(12)
		card.Append(sub)
	}

	if metrics := v.state.Cluster.Metrics.Node(node.Name); metrics != nil {
		bars := gtk.NewBox(gtk.OrientationVertical, 6)
		bars.SetMarginStart(12)
		bars.SetMarginEnd(12)
		bars.SetMarginTop(4)
		bars.Append(resourceRow("CPU", metrics.Usage.Cpu(), node.Status.Allocatable.Cpu(),
			widget.NewResourceBar(metrics.Usage.Cpu(), node.Status.Allocatable.Cpu(), "")))
		bars.Append(resourceRow("Memory", metrics.Usage.Memory(), node.Status.Allocatable.Memory(),
			widget.NewResourceBar(metrics.Usage.Memory(), node.Status.Allocatable.Memory(), "")))
		card.Append(bars)
	}

	running, pending, failed, succeeded, notReady := 0, 0, 0, 0, 0
	for _, p := range pods {
		switch p.Status.Phase {
		case corev1.PodRunning:
			running++
			for _, cs := range p.Status.ContainerStatuses {
				if !cs.Ready {
					notReady++
					break
				}
			}
		case corev1.PodPending:
			pending++
		case corev1.PodFailed:
			failed++
		case corev1.PodSucceeded:
			succeeded++
		}
	}
	sum := gtk.NewBox(gtk.OrientationHorizontal, 4)
	sum.SetMarginStart(12)
	sum.SetMarginEnd(12)
	sum.SetMarginTop(2)
	sum.SetHAlign(gtk.AlignStart)
	if running > 0 {
		sum.Append(countChip(fmt.Sprintf("%d", running), "success", "Running"))
	}
	if notReady > 0 {
		sum.Append(countChip(fmt.Sprintf("%d", notReady), "warning", "Containers not ready"))
	}
	if pending > 0 {
		sum.Append(countChip(fmt.Sprintf("%d", pending), "accent", "Pending"))
	}
	if failed > 0 {
		sum.Append(countChip(fmt.Sprintf("%d", failed), "error", "Failed"))
	}
	if succeeded > 0 {
		sum.Append(countChip(fmt.Sprintf("%d", succeeded), "", "Succeeded"))
	}
	if running+pending+failed+succeeded == 0 {
		sum.Append(dimCaption("no pods"))
	}
	card.Append(sum)

	cap := node.Status.Capacity
	footer := gtk.NewLabel(fmt.Sprintf("%d / %s pods", len(pods), cap.Pods().String()))
	footer.AddCSSClass("caption")
	footer.AddCSSClass("dim-label")
	footer.SetHAlign(gtk.AlignStart)
	footer.SetMarginStart(12)
	footer.SetMarginEnd(12)
	footer.SetMarginBottom(10)
	card.Append(footer)

	gesture := gtk.NewGestureClick()
	gesture.SetButton(gdk.BUTTON_PRIMARY)
	nodeCopy := node
	gesture.ConnectReleased(func(_ int, _, _ float64) {
		v.openObject(nodeCopy)
	})
	card.AddController(gesture)
	card.SetCursorFromName("pointer")
	card.SetTooltipText(nodeTooltip(node, pods))
	return card
}

func (v *Visualizer) makeUnscheduledCard(pods []*corev1.Pod) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 6)
	box.AddCSSClass("card")

	inner := gtk.NewBox(gtk.OrientationVertical, 4)
	inner.SetMarginTop(10)
	inner.SetMarginBottom(10)
	inner.SetMarginStart(12)
	inner.SetMarginEnd(12)

	header := gtk.NewBox(gtk.OrientationHorizontal, 6)
	header.Append(gtk.NewImageFromIconName("dialog-question-symbolic"))
	t := gtk.NewLabel(fmt.Sprintf("%d pods waiting for a node", len(pods)))
	t.AddCSSClass("heading")
	t.SetHAlign(gtk.AlignStart)
	header.Append(t)
	inner.Append(header)

	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	limit := len(pods)
	if limit > 8 {
		limit = 8
	}
	names := make([]string, 0, limit)
	for _, p := range pods[:limit] {
		names = append(names, fmt.Sprintf("%s/%s", p.Namespace, p.Name))
	}
	if len(pods) > limit {
		names = append(names, fmt.Sprintf("+%d more", len(pods)-limit))
	}
	caption := gtk.NewLabel(joinDot(names))
	caption.AddCSSClass("caption")
	caption.AddCSSClass("dim-label")
	caption.SetHAlign(gtk.AlignStart)
	caption.SetWrap(true)
	caption.SetWrapMode(pango.WrapWordChar)
	inner.Append(caption)
	box.Append(inner)
	return box
}

// ---------- Helpers ----------

func resourceRow(label string, used, total *resource.Quantity, bar *gtk.Box) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 2)

	head := gtk.NewBox(gtk.OrientationHorizontal, 6)
	l := gtk.NewLabel(label)
	l.AddCSSClass("caption")
	l.AddCSSClass("dim-label")
	l.SetHAlign(gtk.AlignStart)
	l.SetHExpand(true)
	l.SetXAlign(0)
	head.Append(l)

	pct := "—"
	if used != nil && total != nil {
		totF := total.AsApproximateFloat64()
		if totF > 0 {
			pct = fmt.Sprintf("%.0f%%", used.AsApproximateFloat64()/totF*100)
		}
	}
	r := gtk.NewLabel(pct)
	r.AddCSSClass("caption")
	r.AddCSSClass("dim-label")
	r.SetHAlign(gtk.AlignEnd)
	head.Append(r)

	box.Append(head)
	bar.SetHExpand(true)
	box.Append(bar)
	return box
}

func countChip(text, semantic, tooltip string) *gtk.Label {
	l := gtk.NewLabel("● " + text)
	l.AddCSSClass("pill")
	l.AddCSSClass("caption")
	if semantic != "" {
		l.AddCSSClass(semantic)
	} else {
		l.AddCSSClass("dim-label")
	}
	if tooltip != "" {
		l.SetTooltipText(tooltip)
	}
	return l
}

func dimCaption(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.AddCSSClass("caption")
	l.AddCSSClass("dim-label")
	return l
}

func pillLabel(text, semantic, tooltip string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.AddCSSClass("pill")
	l.AddCSSClass("caption")
	if semantic != "" {
		l.AddCSSClass(semantic)
	}
	if tooltip != "" {
		l.SetTooltipText(tooltip)
	}
	return l
}

func joinDot(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " • "
		}
		out += p
	}
	return out
}

func (v *Visualizer) openObject(obj interface {
	GetName() string
}) {
	switch o := obj.(type) {
	case *corev1.Pod:
		v.state.SelectedObject.Pub(o)
	case *corev1.Node:
		v.state.SelectedObject.Pub(o)
	default:
		return
	}
	v.dialog.Present(v)
}

func isControlPlane(n *corev1.Node) bool {
	for k := range n.Labels {
		if k == "node-role.kubernetes.io/control-plane" || k == "node-role.kubernetes.io/master" {
			return true
		}
	}
	return false
}

func nodeReadiness(n *corev1.Node) (bool, string) {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, c.Message
		}
	}
	return false, "no Ready condition reported"
}

func nodeTooltip(n *corev1.Node, pods []*corev1.Pod) string {
	cap := n.Status.Capacity
	age := "—"
	if !n.CreationTimestamp.IsZero() {
		age = util.HumanizeApproximateDuration(time.Since(n.CreationTimestamp.Time))
	}
	return fmt.Sprintf("%s\nCPU %s • Mem %s • Pods %d/%s\nAge %s",
		n.Name, cap.Cpu().String(), cap.Memory().String(), len(pods), cap.Pods().String(), age)
}
