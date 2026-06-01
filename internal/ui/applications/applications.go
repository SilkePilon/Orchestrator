// Package applications renders a GNOME-styled adaptation of Portainer's
// Applications list: workloads (Deployments / StatefulSets / DaemonSets) shown
// grouped by Helm release, with status, type, image, namespace and creation
// metadata, and click-through to the existing single-object bottom sheet.
package applications

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/zmwangx/debounce"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	deploymentsGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	statefulSetsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	daemonSetsGVR   = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	servicesGVR     = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	namespacesGVR   = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
)

const (
	helmManagedByValue = "Helm"
	helmInstanceLabel  = "app.kubernetes.io/instance"
	helmNameLabel      = "app.kubernetes.io/name"
	managedByLabel     = "app.kubernetes.io/managed-by"
	helmChartLabel     = "helm.sh/chart"
	helmReleaseAnnot   = "meta.helm.sh/release-name"
	helmNamespaceAnnot = "meta.helm.sh/release-namespace"
)

// Applications is the top-level widget for the view.
type Applications struct {
	*adw.ToolbarView

	ctx    context.Context
	state  *common.ClusterState
	dialog *adw.Dialog

	search       *gtk.SearchEntry
	nsMenuSection *gio.Menu
	body         *gtk.Box
	scroll       *gtk.ScrolledWindow
	emptyStatus  *adw.StatusPage
	rebuild      func()
	currentNS    string
	currentQuery string
	showSystem   bool
}

// NewApplications constructs the Applications view.
func NewApplications(ctx context.Context, state *common.ClusterState, dialog *adw.Dialog) *Applications {
	a := &Applications{
		ToolbarView: adw.NewToolbarView(),
		ctx:         ctx,
		state:       state,
		dialog:      dialog,
	}
	a.AddCSSClass("view")

	header := adw.NewHeaderBar()
	header.AddCSSClass("flat")

	// Linked search entry + filter popover (matches the Pods view header).
	linked := gtk.NewBox(gtk.OrientationHorizontal, 0)
	linked.AddCSSClass("linked")
	linked.SetMarginStart(32)
	linked.SetMarginEnd(32)
	header.SetTitleWidget(linked)

	a.search = gtk.NewSearchEntry()
	a.search.SetMaxWidthChars(75)
	a.search.SetHExpand(true)
	a.search.SetObjectProperty("placeholder-text", "Search")
	linked.Append(a.search)
	debouncedSearch, _ := debounce.Debounce(func() {
		glib.IdleAdd(func() {
			a.currentQuery = strings.TrimSpace(strings.ToLower(a.search.Text()))
			a.render()
		})
	}, 150*time.Millisecond)
	a.search.ConnectSearchChanged(func() { debouncedSearch() })

	filterButton := gtk.NewMenuButton()
	filterButton.SetIconName("funnel-symbolic")
	filterButton.SetTooltipText("Filter")
	linked.Append(filterButton)

	// Stateful actions backing the popover menu.
	nsAction := gio.NewSimpleActionStateful("filterNamespace",
		glib.NewVariantType("s"), glib.NewVariantString(""))
	nsAction.ConnectChangeState(func(value *glib.Variant) {
		if value == nil {
			return
		}
		a.currentNS = value.String()
		nsAction.SetState(value)
		a.render()
	})
	sysAction := gio.NewSimpleActionStateful("showSystem",
		nil, glib.NewVariantBoolean(false))
	sysAction.ConnectChangeState(func(value *glib.Variant) {
		if value == nil {
			return
		}
		a.showSystem = value.Boolean()
		sysAction.SetState(value)
		a.render()
	})
	group := gio.NewSimpleActionGroup()
	group.AddAction(nsAction)
	group.AddAction(sysAction)
	a.InsertActionGroup("apps", group)

	a.nsMenuSection = gio.NewMenu()
	optionsSection := gio.NewMenu()
	optionsSection.Append("Show system namespaces", "apps.showSystem")
	rootMenu := gio.NewMenu()
	rootMenu.AppendSection("Namespace", a.nsMenuSection)
	rootMenu.AppendSection("Options", optionsSection)
	filterButton.SetMenuModel(rootMenu)

	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.AddCSSClass("flat")
	refresh.SetTooltipText("Refresh")
	refresh.ConnectClicked(func() { a.render() })
	header.PackEnd(refresh)
	a.AddTopBar(header)

	// Body.
	a.body = gtk.NewBox(gtk.OrientationVertical, 0)
	a.body.SetMarginTop(24)
	a.body.SetMarginBottom(18)
	a.body.SetMarginStart(18)
	a.body.SetMarginEnd(18)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(1100)
	clamp.SetTighteningThreshold(900)
	clamp.SetChild(a.body)

	a.scroll = gtk.NewScrolledWindow()
	a.scroll.SetHExpand(true)
	a.scroll.SetVExpand(true)
	a.scroll.SetChild(clamp)
	a.SetContent(a.scroll)

	debounced, _ := debounce.Debounce(func() {
		glib.IdleAdd(a.render)
	}, 250*time.Millisecond)
	a.rebuild = debounced

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { a.rebuild() },
		UpdateFunc: func(_, _ interface{}) { a.rebuild() },
		DeleteFunc: func(_ interface{}) { a.rebuild() },
	}
	for _, gvr := range []schema.GroupVersionResource{deploymentsGVR, statefulSetsGVR, daemonSetsGVR, servicesGVR, namespacesGVR} {
		if err := state.Cluster.AddInformerEventHandler(ctx, gvr, handler); err != nil {
			klog.Warningf("applications: %s watch: %v", gvr.Resource, err)
		}
	}

	state.Namespaces.Sub(ctx, func(_ []*corev1.Namespace) {
		glib.IdleAdd(a.refreshNamespaceList)
	})

	a.refreshNamespaceList()
	a.placeholder("Loading workloads…")
	a.rebuild()
	return a
}

// ---------- Data types ----------

type workloadKind string

const (
	kindDeployment  workloadKind = "Deployment"
	kindStatefulSet workloadKind = "StatefulSet"
	kindDaemonSet   workloadKind = "DaemonSet"
)

// workload is the unified shape we render in the table.
type workload struct {
	obj       client.Object
	kind      workloadKind
	name      string
	namespace string
	image     string
	created   time.Time
	ready     int32
	desired   int32
	stack     string // helm release if any, else "-"
	helm      string // non-empty if part of a helm release (release name)
	published bool
}

func (w *workload) statusText() string {
	switch w.kind {
	case kindDaemonSet:
		return fmt.Sprintf("%d / %d", w.ready, w.desired)
	default:
		return fmt.Sprintf("%d / %d", w.ready, w.desired)
	}
}

func (w *workload) statusOK() bool {
	return w.desired > 0 && w.ready == w.desired
}

// helmRelease aggregates the workloads belonging to one Helm release.
type helmRelease struct {
	name      string
	namespace string
	chart     string
	created   time.Time
	image     string
	workloads []*workload
}

func (r *helmRelease) ok() bool {
	for _, w := range r.workloads {
		if !w.statusOK() {
			return false
		}
	}
	return len(r.workloads) > 0
}

func (r *helmRelease) anyPublished() bool {
	for _, w := range r.workloads {
		if w.published {
			return true
		}
	}
	return false
}

// ---------- Render ----------

func (a *Applications) refreshNamespaceList() {
	a.nsMenuSection.RemoveAll()
	all := gio.NewMenuItem("All namespaces", "")
	all.SetActionAndTargetValue("apps.filterNamespace", glib.NewVariantString(""))
	a.nsMenuSection.AppendItem(all)

	names := make([]string, 0)
	for _, ns := range a.state.Namespaces.Value() {
		names = append(names, ns.Name)
	}
	sort.Strings(names)
	stillExists := a.currentNS == ""
	for _, n := range names {
		if n == a.currentNS {
			stillExists = true
		}
		item := gio.NewMenuItem(n, "")
		item.SetActionAndTargetValue("apps.filterNamespace", glib.NewVariantString(n))
		a.nsMenuSection.AppendItem(item)
	}
	if !stillExists {
		a.currentNS = ""
	}
}

func (a *Applications) clear() {
	for child := a.body.FirstChild(); child != nil; child = a.body.FirstChild() {
		a.body.Remove(child)
	}
}

func (a *Applications) placeholder(text string) {
	a.clear()
	status := adw.NewStatusPage()
	status.SetIconName("application-x-executable-symbolic")
	status.SetTitle(text)
	status.SetVExpand(true)
	a.body.Append(status)
}

func (a *Applications) render() {
	cluster := a.state.Cluster

	var deployments []*appsv1.Deployment
	listInto(cluster, deploymentsGVR, func(o interface{}) {
		if d, ok := o.(*appsv1.Deployment); ok {
			deployments = append(deployments, d)
		}
	})
	var statefulSets []*appsv1.StatefulSet
	listInto(cluster, statefulSetsGVR, func(o interface{}) {
		if s, ok := o.(*appsv1.StatefulSet); ok {
			statefulSets = append(statefulSets, s)
		}
	})
	var daemonSets []*appsv1.DaemonSet
	listInto(cluster, daemonSetsGVR, func(o interface{}) {
		if d, ok := o.(*appsv1.DaemonSet); ok {
			daemonSets = append(daemonSets, d)
		}
	})
	var services []*corev1.Service
	listInto(cluster, servicesGVR, func(o interface{}) {
		if s, ok := o.(*corev1.Service); ok {
			services = append(services, s)
		}
	})

	workloads := make([]*workload, 0, len(deployments)+len(statefulSets)+len(daemonSets))
	for _, d := range deployments {
		workloads = append(workloads, fromDeployment(d))
	}
	for _, s := range statefulSets {
		workloads = append(workloads, fromStatefulSet(s))
	}
	for _, d := range daemonSets {
		workloads = append(workloads, fromDaemonSet(d))
	}

	// Compute "Published": at least one Service in the same namespace
	// of type NodePort/LoadBalancer with a selector matching the workload's
	// pod template labels.
	for _, w := range workloads {
		w.published = isPublished(w, services)
	}

	// Filter by namespace and search.
	systemOK := a.showSystem
	filtered := workloads[:0]
	for _, w := range workloads {
		if !systemOK && isSystemNamespace(w.namespace) {
			continue
		}
		if a.currentNS != "" && w.namespace != a.currentNS {
			continue
		}
		if a.currentQuery != "" {
			hay := strings.ToLower(w.name + " " + w.namespace + " " + w.image + " " + w.stack)
			if !strings.Contains(hay, a.currentQuery) {
				continue
			}
		}
		filtered = append(filtered, w)
	}

	// Group: helm releases and standalone workloads.
	releases := map[string]*helmRelease{}
	standalone := []*workload{}
	for _, w := range filtered {
		if w.helm == "" {
			standalone = append(standalone, w)
			continue
		}
		key := w.namespace + "/" + w.helm
		r := releases[key]
		if r == nil {
			r = &helmRelease{
				name:      w.helm,
				namespace: w.namespace,
				created:   w.created,
				image:     w.image,
			}
			releases[key] = r
		}
		r.workloads = append(r.workloads, w)
		if w.created.Before(r.created) || r.created.IsZero() {
			r.created = w.created
		}
	}

	a.clear()

	if len(filtered) == 0 {
		hint := "No workloads match the current filters."
		if len(workloads) == 0 {
			hint = "No Deployments, StatefulSets or DaemonSets found in the cluster."
		}
		a.placeholder(hint)
		return
	}

	// Build a single PreferencesGroup that contains both helm releases
	// (ExpanderRows) and standalone workloads (ActionRows). This mirrors
	// Portainer's flat-then-nested presentation while staying GNOME-native.
	type entry struct {
		name    string
		created time.Time
		row     gtk.Widgetter
	}
	entries := make([]entry, 0, len(releases)+len(standalone))
	for _, r := range releases {
		sort.Slice(r.workloads, func(i, j int) bool { return r.workloads[i].name < r.workloads[j].name })
		entries = append(entries, entry{name: r.name, created: r.created, row: a.makeReleaseRow(r)})
	}
	for _, w := range standalone {
		entries = append(entries, entry{name: w.name, created: w.created, row: a.makeWorkloadRow(w, false)})
	}
	// Newest first, fall back to name.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].created.Equal(entries[j].created) {
			return entries[i].name < entries[j].name
		}
		return entries[i].created.After(entries[j].created)
	})

	group := adw.NewPreferencesGroup()
	for _, e := range entries {
		group.Add(e.row)
	}
	a.body.Append(group)
}

// ---------- Row builders ----------

func (a *Applications) makeReleaseRow(r *helmRelease) gtk.Widgetter {
	exp := adw.NewExpanderRow()
	exp.SetTitle(escapeMarkup(r.name))
	subtitle := fmt.Sprintf("%s • %d workload", r.namespace, len(r.workloads))
	if len(r.workloads) != 1 {
		subtitle += "s"
	}
	if !r.created.IsZero() {
		subtitle += " • " + util.HumanizeApproximateDuration(time.Since(r.created)) + " ago"
	}
	exp.SetSubtitle(escapeMarkup(subtitle))

	// Suffixes: Type pill (Helm), Status pill, Published pill (if any).
	exp.AddSuffix(typePill("Helm"))
	if r.ok() {
		exp.AddSuffix(statusPill("Ready", "success"))
	} else {
		exp.AddSuffix(statusPill("Degraded", "warning"))
	}
	if r.anyPublished() {
		exp.AddSuffix(publishedBadge())
	}

	for _, w := range r.workloads {
		exp.AddRow(a.makeWorkloadRow(w, true))
	}
	return exp
}

func (a *Applications) makeWorkloadRow(w *workload, nested bool) gtk.Widgetter {
	row := adw.NewActionRow()
	row.SetTitle(escapeMarkup(w.name))

	subtitleParts := []string{}
	if !nested {
		subtitleParts = append(subtitleParts, w.namespace)
	}
	if w.image != "" {
		subtitleParts = append(subtitleParts, w.image)
	}
	if !w.created.IsZero() {
		subtitleParts = append(subtitleParts, util.HumanizeApproximateDuration(time.Since(w.created))+" ago")
	}
	row.SetSubtitle(escapeMarkup(strings.Join(subtitleParts, " • ")))
	row.SetSubtitleLines(1)

	row.AddSuffix(typePill(string(w.kind)))

	statusText := w.statusText()
	if w.statusOK() {
		row.AddSuffix(statusPill("● "+statusText, "success"))
	} else if w.desired == 0 {
		row.AddSuffix(statusPill("● scaled to 0", "dim-label"))
	} else {
		row.AddSuffix(statusPill("● "+statusText, "warning"))
	}
	if w.published {
		row.AddSuffix(publishedBadge())
	}

	open := gtk.NewButtonFromIconName("go-next-symbolic")
	open.AddCSSClass("flat")
	open.SetVAlign(gtk.AlignCenter)
	open.SetTooltipText("Open details")
	objCopy := w.obj
	open.ConnectClicked(func() { a.openObject(objCopy) })
	row.AddSuffix(open)

	row.SetActivatable(true)
	row.ConnectActivated(func() { a.openObject(objCopy) })

	// Right-click also opens (cheap affordance).
	gesture := gtk.NewGestureClick()
	gesture.SetButton(gdk.BUTTON_SECONDARY)
	gesture.ConnectReleased(func(_ int, _, _ float64) { a.openObject(objCopy) })
	row.AddController(gesture)
	return row
}

func (a *Applications) openObject(obj client.Object) {
	if obj == nil {
		return
	}
	a.state.SelectedObject.Pub(obj)
	a.dialog.Present(a)
}

// ---------- Conversion helpers ----------

func fromDeployment(d *appsv1.Deployment) *workload {
	w := &workload{
		obj:       d,
		kind:      kindDeployment,
		name:      d.Name,
		namespace: d.Namespace,
		image:     firstImage(d.Spec.Template.Spec.Containers),
		created:   d.CreationTimestamp.Time,
		ready:     d.Status.AvailableReplicas,
		desired:   d.Status.Replicas,
	}
	w.helm = helmReleaseOf(&d.ObjectMeta)
	w.stack = stackName(d.Name, w.helm)
	return w
}

func fromStatefulSet(s *appsv1.StatefulSet) *workload {
	w := &workload{
		obj:       s,
		kind:      kindStatefulSet,
		name:      s.Name,
		namespace: s.Namespace,
		image:     firstImage(s.Spec.Template.Spec.Containers),
		created:   s.CreationTimestamp.Time,
		ready:     s.Status.ReadyReplicas,
		desired:   s.Status.Replicas,
	}
	w.helm = helmReleaseOf(&s.ObjectMeta)
	w.stack = stackName(s.Name, w.helm)
	return w
}

func fromDaemonSet(d *appsv1.DaemonSet) *workload {
	w := &workload{
		obj:       d,
		kind:      kindDaemonSet,
		name:      d.Name,
		namespace: d.Namespace,
		image:     firstImage(d.Spec.Template.Spec.Containers),
		created:   d.CreationTimestamp.Time,
		ready:     d.Status.NumberReady,
		desired:   d.Status.DesiredNumberScheduled,
	}
	w.helm = helmReleaseOf(&d.ObjectMeta)
	w.stack = stackName(d.Name, w.helm)
	return w
}

func firstImage(c []corev1.Container) string {
	if len(c) == 0 {
		return ""
	}
	return c[0].Image
}

func helmReleaseOf(meta *metav1.ObjectMeta) string {
	if meta.Labels[managedByLabel] == helmManagedByValue {
		if v := meta.Labels[helmInstanceLabel]; v != "" {
			return v
		}
	}
	if v := meta.Annotations[helmReleaseAnnot]; v != "" {
		return v
	}
	return ""
}

func stackName(name, helm string) string {
	if helm != "" {
		return "-"
	}
	return name
}

func isPublished(w *workload, svcs []*corev1.Service) bool {
	tmplLabels := podTemplateLabels(w.obj)
	if len(tmplLabels) == 0 {
		return false
	}
	for _, s := range svcs {
		if s.Namespace != w.namespace {
			continue
		}
		if s.Spec.Type != corev1.ServiceTypeNodePort && s.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		if len(s.Spec.Selector) == 0 {
			continue
		}
		if labels.SelectorFromSet(s.Spec.Selector).Matches(labels.Set(tmplLabels)) {
			return true
		}
	}
	return false
}

func podTemplateLabels(obj client.Object) map[string]string {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return o.Spec.Template.Labels
	case *appsv1.StatefulSet:
		return o.Spec.Template.Labels
	case *appsv1.DaemonSet:
		return o.Spec.Template.Labels
	}
	return nil
}

func isSystemNamespace(ns string) bool {
	switch ns {
	case "kube-system", "kube-public", "kube-node-lease", "kubernetes-dashboard":
		return true
	}
	return strings.HasPrefix(ns, "kube-")
}

// ---------- Small widget helpers ----------

func typePill(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.AddCSSClass("pill")
	l.AddCSSClass("caption")
	l.AddCSSClass("accent")
	l.SetVAlign(gtk.AlignCenter)
	return l
}

func statusPill(text, semantic string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.AddCSSClass("pill")
	l.AddCSSClass("caption")
	if semantic != "" {
		l.AddCSSClass(semantic)
	}
	l.SetVAlign(gtk.AlignCenter)
	l.SetEllipsize(pango.EllipsizeEnd)
	l.SetMaxWidthChars(20)
	return l
}

func publishedBadge() *gtk.Label {
	l := gtk.NewLabel("Published")
	l.AddCSSClass("pill")
	l.AddCSSClass("caption")
	l.AddCSSClass("success")
	l.SetTooltipText("Exposed via a NodePort or LoadBalancer service")
	l.SetVAlign(gtk.AlignCenter)
	return l
}

func escapeMarkup(s string) string {
	// adw rows use markup for title/subtitle; escape just in case.
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func listInto(cluster *api.Cluster, gvr schema.GroupVersionResource, fn func(interface{})) {
	inf := cluster.GetInformer(gvr)
	if inf == nil {
		return
	}
	if err := cache.ListAll(inf.Informer().GetIndexer(), labels.Everything(), fn); err != nil {
		klog.Warningf("applications: list %s: %v", gvr.Resource, err)
	}
}
