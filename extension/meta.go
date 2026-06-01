package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	Extensions = append(Extensions, NewMeta)
}

func NewMeta(_ context.Context, cluster *api.Cluster) (Extension, error) {
	return &Meta{Cluster: cluster}, nil
}

type Meta struct {
	Noop
	*api.Cluster
}

func (e *Meta) CreateColumns(ctx context.Context, resource *metav1.APIResource, columns []api.Column) []api.Column {
	columns = append(columns, api.Column{
		Name:     "Name",
		Priority: 100,
		Bind: func(cell api.Cell, object client.Object) {
			label := gtk.NewLabel(object.GetName())
			label.SetHAlign(gtk.AlignStart)
			label.SetEllipsize(pango.EllipsizeEnd)

			port, ok := lookupForwardedPort(e.Cluster, object)
			if !ok {
				cell.SetChild(label)
				return
			}

			box := gtk.NewBox(gtk.OrientationHorizontal, 4)
			box.SetHAlign(gtk.AlignStart)
			label.SetHAlign(gtk.AlignStart)
			box.Append(label)

			url := fmt.Sprintf("http://localhost:%d", port)
			icon := gtk.NewImageFromIconName("external-link-symbolic")
			icon.SetPixelSize(12)
			icon.AddCSSClass("dim-label")
			icon.SetVAlign(gtk.AlignCenter)
			icon.SetTooltipText("Open " + url)
			icon.SetCursorFromName("pointer")
			click := gtk.NewGestureClick()
			click.ConnectReleased(func(_ int, _, _ float64) {
				gtk.ShowURI(nil, url, gdk.CURRENT_TIME)
			})
			icon.AddController(click)
			box.Append(icon)
			cell.SetChild(box)
		},
		Compare: func(a, b client.Object) int {
			return strings.Compare(a.GetName(), b.GetName())
		},
	})

	if resource.Namespaced {
		columns = append(columns, api.Column{
			Name:     "Namespace",
			Priority: 90,
			Bind: func(cell api.Cell, object client.Object) {
				cell.SetLabel(object.GetNamespace())
			},
			Compare: func(a, b client.Object) int {
				return strings.Compare(a.GetNamespace(), b.GetNamespace())
			},
		})
	}

	columns = append(columns, api.Column{
		Name:     "Age",
		Priority: 80,
		Bind: func(cell api.Cell, object client.Object) {
			duration := time.Since(object.GetCreationTimestamp().Time)
			cell.SetLabel(util.HumanizeApproximateDuration(duration))
		},
		Compare: func(a, b client.Object) int {
			return a.GetCreationTimestamp().Compare(b.GetCreationTimestamp().Time)
		},
	})

	return columns
}

func (e *Meta) CreateObjectProperties(ctx context.Context, resource *metav1.APIResource, object client.Object, props []api.Property) []api.Property {
	readOnly := e.ClusterPreferences.Value().ReadOnly

	var labels []api.Property
	for key, value := range object.GetLabels() {
		key, value := key, value
		prop := &api.TextProperty{Name: key, Value: value}
		if !readOnly {
			prop.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
				row, ok := w.(*adw.ActionRow)
				if !ok {
					return
				}
				editBtn := gtk.NewButtonFromIconName("edit-symbolic")
				editBtn.AddCSSClass("flat")
				editBtn.SetVAlign(gtk.AlignCenter)
				editBtn.SetTooltipText("Edit label")
				editBtn.ConnectClicked(func() {
					showEditDialog(ctx, "Edit Label: "+key, value, func(newVal string) {
						patchData, _ := json.Marshal(map[string]any{
							"metadata": map[string]any{
								"labels": map[string]any{key: newVal},
							},
						})
						if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
							widget.ShowErrorDialog(ctx, "Failed to update label", err)
							return
						}
						ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Label updated"))
					})
				})
				row.AddSuffix(editBtn)

				deleteBtn := gtk.NewButtonFromIconName("edit-delete-symbolic")
				deleteBtn.AddCSSClass("flat")
				deleteBtn.AddCSSClass("error")
				deleteBtn.SetVAlign(gtk.AlignCenter)
				deleteBtn.SetTooltipText("Remove label")
				deleteBtn.ConnectClicked(func() {
					patchData, _ := json.Marshal(map[string]any{
						"metadata": map[string]any{
							"labels": map[string]any{key: nil},
						},
					})
					if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to remove label", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Label removed"))
				})
				row.AddSuffix(deleteBtn)
			}
		}
		labels = append(labels, prop)
	}

	var annotations []api.Property
	for key, value := range object.GetAnnotations() {
		key, value := key, value
		prop := &api.TextProperty{Name: key, Value: value}
		if !readOnly {
			prop.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
				row, ok := w.(*adw.ActionRow)
				if !ok {
					return
				}
				editBtn := gtk.NewButtonFromIconName("edit-symbolic")
				editBtn.AddCSSClass("flat")
				editBtn.SetVAlign(gtk.AlignCenter)
				editBtn.SetTooltipText("Edit annotation")
				editBtn.ConnectClicked(func() {
					showEditDialog(ctx, "Edit Annotation: "+key, value, func(newVal string) {
						patchData, _ := json.Marshal(map[string]any{
							"metadata": map[string]any{
								"annotations": map[string]any{key: newVal},
							},
						})
						if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
							widget.ShowErrorDialog(ctx, "Failed to update annotation", err)
							return
						}
						ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Annotation updated"))
					})
				})
				row.AddSuffix(editBtn)

				deleteBtn := gtk.NewButtonFromIconName("edit-delete-symbolic")
				deleteBtn.AddCSSClass("flat")
				deleteBtn.AddCSSClass("error")
				deleteBtn.SetVAlign(gtk.AlignCenter)
				deleteBtn.SetTooltipText("Remove annotation")
				deleteBtn.ConnectClicked(func() {
					patchData, _ := json.Marshal(map[string]any{
						"metadata": map[string]any{
							"annotations": map[string]any{key: nil},
						},
					})
					if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to remove annotation", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Annotation removed"))
				})
				row.AddSuffix(deleteBtn)
			}
		}
		annotations = append(annotations, prop)
	}
	var owners []api.Property
	for _, ref := range object.GetOwnerReferences() {
		owners = append(owners, &api.TextProperty{
			Name:  fmt.Sprintf("%s %s", ref.APIVersion, ref.Kind),
			Value: ref.Name,
			Reference: &corev1.ObjectReference{
				APIVersion: ref.APIVersion,
				Kind:       ref.Kind,
				Name:       ref.Name,
				UID:        ref.UID,
				Namespace:  object.GetNamespace(),
			},
		})
	}

	group := object.GetObjectKind().GroupVersionKind().Group
	if len(group) == 0 {
		group = "k8s.io"
	}

	metadata := api.GroupProperty{
		Priority: 100,
		Name:     "Metadata",
		Children: []api.Property{
			&api.TextProperty{
				Name:  "Name",
				Value: object.GetName(),
			},
			&api.TextProperty{
				Name:  "Namespace",
				Value: object.GetNamespace(),
			},
			&api.TextProperty{
				Name:  "Created",
				Value: object.GetCreationTimestamp().Format(time.RFC822),
			},
			// &api.TextProperty{
			// 	Name:  "Kind",
			// 	Value: object.GetObjectKind().GroupVersionKind().Kind,
			// },
			// &api.TextProperty{
			// 	Name:  "Group",
			// 	Value: group,
			// },
			e.labelsGroup(ctx, object, labels, readOnly),
			e.annotationsGroup(ctx, object, annotations, readOnly),
			&api.GroupProperty{
				Name:     "Owners",
				Children: owners,
			},
		},
	}
	if !resource.Namespaced {
		metadata.Children = slices.Delete(metadata.Children, 1, 2)
	}
	props = append(props, &metadata)

	events := &api.GroupProperty{Name: "Events", Priority: -100}
	for _, ev := range e.Events.For(object) {
		eventTime := ev.EventTime.Time
		if eventTime.IsZero() {
			eventTime = ev.CreationTimestamp.Time
		}
		events.Children = append(events.Children, &api.TextProperty{
			Name:  eventTime.Format(time.RFC822),
			Value: ev.Note,
		})
	}
	if len(events.Children) > 0 {
		props = append(props, events)
	}

	return props
}

// lookupForwardedPort returns a local port to open in the browser when an
// active port-forward exists for the given object. Pods match directly;
// Deployments resolve to any backing pod with an active forward.
func lookupForwardedPort(cluster *api.Cluster, object client.Object) (uint16, bool) {
	pf := PortForwarderFor(cluster)
	if pf == nil {
		return 0, false
	}
	nn := types.NamespacedName{Name: object.GetName(), Namespace: object.GetNamespace()}
	switch object.(type) {
	case *corev1.Pod:
		return pf.LocalPortForPod(nn)
	case *appsv1.Deployment:
		return pf.LocalPortForDeployment(nn)
	}
	return 0, false
}

// labelsGroup returns the Labels GroupProperty with optional add/edit/delete actions.
func (e *Meta) labelsGroup(ctx context.Context, object client.Object, labels []api.Property, readOnly bool) *api.GroupProperty {
	g := &api.GroupProperty{Name: "Labels", Children: labels}
	if !readOnly {
		g.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
			row, ok := w.(*adw.ExpanderRow)
			if !ok {
				return
			}
			row.SetSensitive(true)
			addBtn := gtk.NewButtonFromIconName("list-add-symbolic")
			addBtn.AddCSSClass("flat")
			addBtn.SetVAlign(gtk.AlignCenter)
			addBtn.SetTooltipText("Add label")
			addBtn.ConnectClicked(func() {
				showAddPairDialog(ctx, "Add Label", "key", "value", func(k, v string) {
					patchData, _ := json.Marshal(map[string]any{
						"metadata": map[string]any{
							"labels": map[string]any{k: v},
						},
					})
					if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to add label", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Label added"))
				})
			})
			row.AddSuffix(addBtn)
		}
	}
	return g
}

// annotationsGroup returns the Annotations GroupProperty with optional add/edit/delete actions.
func (e *Meta) annotationsGroup(ctx context.Context, object client.Object, annotations []api.Property, readOnly bool) *api.GroupProperty {
	g := &api.GroupProperty{Name: "Annotations", Children: annotations}
	if !readOnly {
		g.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
			row, ok := w.(*adw.ExpanderRow)
			if !ok {
				return
			}
			row.SetSensitive(true)
			addBtn := gtk.NewButtonFromIconName("list-add-symbolic")
			addBtn.AddCSSClass("flat")
			addBtn.SetVAlign(gtk.AlignCenter)
			addBtn.SetTooltipText("Add annotation")
			addBtn.ConnectClicked(func() {
				showAddPairDialog(ctx, "Add Annotation", "key", "value", func(k, v string) {
					patchData, _ := json.Marshal(map[string]any{
						"metadata": map[string]any{
							"annotations": map[string]any{k: v},
						},
					})
					if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to add annotation", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Annotation added"))
				})
			})
			row.AddSuffix(addBtn)
		}
	}
	return g
}

// IsNodeControlPlane reports whether a Node carries the control-plane role
// label (or its legacy "master" form).
func IsNodeControlPlane(node *corev1.Node) bool {
	for k := range node.Labels {
		if k == "node-role.kubernetes.io/control-plane" || k == "node-role.kubernetes.io/master" {
			return true
		}
	}
	return false
}
