package extension

import (
	"context"
	"fmt"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/reference"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	Extensions = append(Extensions, NewApps)
}

func NewApps(_ context.Context, cluster *api.Cluster) (Extension, error) {
	return &Apps{Cluster: cluster}, nil
}

type Apps struct {
	Noop
	*api.Cluster
}

func (e *Apps) CreateColumns(ctx context.Context, resource *metav1.APIResource, columns []api.Column) []api.Column {
	switch util.GVRForResource(resource).String() {
	case appsv1.SchemeGroupVersion.WithResource("deployments").String():
		columns = append(columns,
			api.Column{
				Name:     "Status",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					cell.SetChild(api.NewStatusWithObject(object).Icon())
				},
				Compare: api.CompareObjectStatus,
			},
			api.Column{
				Name:     "Available",
				Priority: 60,
				Bind: func(cell api.Cell, object client.Object) {
					deployment := object.(*appsv1.Deployment)
					cell.SetLabel(fmt.Sprintf("%d/%d", deployment.Status.AvailableReplicas, deployment.Status.Replicas))
				},
			},
		)
	case appsv1.SchemeGroupVersion.WithResource("replicasets").String():
		columns = append(columns,
			api.Column{
				Name:     "Status",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					cell.SetChild(api.NewStatusWithObject(object).Icon())
				},
				Compare: api.CompareObjectStatus,
			},
			api.Column{
				Name:     "Available",
				Priority: 60,
				Bind: func(cell api.Cell, object client.Object) {
					replicaSet := object.(*appsv1.ReplicaSet)
					cell.SetLabel("%d/%d", replicaSet.Status.AvailableReplicas, replicaSet.Status.Replicas)
				},
			},
		)
	case appsv1.SchemeGroupVersion.WithResource("statefulsets").String():
		columns = append(columns,
			api.Column{
				Name:     "Status",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					cell.SetChild(api.NewStatusWithObject(object).Icon())
				},
				Compare: api.CompareObjectStatus,
			},
			api.Column{
				Name:     "Available",
				Priority: 60,
				Bind: func(cell api.Cell, object client.Object) {
					statefulSet := object.(*appsv1.StatefulSet)
					cell.SetLabel("%d/%d", statefulSet.Status.AvailableReplicas, statefulSet.Status.Replicas)
				},
			},
		)
	}

	return columns
}

func (e *Apps) CreateObjectProperties(ctx context.Context, _ *metav1.APIResource, object client.Object, props []api.Property) []api.Property {
	switch object := object.(type) {
	case *appsv1.Deployment:
		props = append(props, e.workloadActionsProperty(ctx, object, func() int32 { return ptr.Deref(object.Spec.Replicas, 1) }, true))
		prop := &api.GroupProperty{Name: "Pods"}
		var pods corev1.PodList
		e.List(ctx, &pods, client.InNamespace(object.Namespace), client.MatchingLabels(object.Spec.Selector.MatchLabels))
		for i, pod := range pods.Items {
			ref, _ := reference.GetReference(e.Scheme, &pod)
			prop.Children = append(prop.Children, &api.TextProperty{
				ID:        fmt.Sprintf("pods.%d", i),
				Reference: ref,
				Value:     pod.Name,
				Widget: func(w gtk.Widgetter, nv *adw.NavigationView) {
					switch row := w.(type) {
					case *adw.ActionRow:
						row.AddPrefix(api.NewStatusWithObject(&pod).Icon())
					}
				},
			})
		}
		props = append(props, prop)
	case *appsv1.ReplicaSet:
		props = append(props, e.workloadActionsProperty(ctx, object, func() int32 { return ptr.Deref(object.Spec.Replicas, 1) }, false))
		prop := &api.GroupProperty{Name: "Pods"}
		var pods corev1.PodList
		e.List(ctx, &pods, client.InNamespace(object.Namespace), client.MatchingLabels(object.Spec.Selector.MatchLabels))
		// TODO should we also filter pods by owner? takes one more api call to fetch replicasets
		for i, pod := range pods.Items {
			ref, _ := reference.GetReference(e.Scheme, &pod)
			prop.Children = append(prop.Children, &api.TextProperty{
				ID:        fmt.Sprintf("pods.%d", i),
				Reference: ref,
				Value:     pod.Name,
				Widget: func(w gtk.Widgetter, nv *adw.NavigationView) {
					switch row := w.(type) {
					case *adw.ActionRow:
						row.AddPrefix(api.NewStatusWithObject(&pod).Icon())
					}
				},
			})
		}
		props = append(props, prop)
	case *appsv1.StatefulSet:
		props = append(props, e.workloadActionsProperty(ctx, object, func() int32 { return ptr.Deref(object.Spec.Replicas, 1) }, true))
		podsProp := &api.GroupProperty{Name: "Pods"}
		var pods corev1.PodList
		e.List(ctx, &pods, client.InNamespace(object.Namespace), client.MatchingLabels(object.Spec.Selector.MatchLabels))
		for i, pod := range pods.Items {
			var ok bool
			for _, owner := range pod.OwnerReferences {
				if owner.UID == object.UID {
					ok = true
				}
			}
			if !ok {
				continue
			}
			ref, _ := reference.GetReference(e.Scheme, &pod)
			podsProp.Children = append(podsProp.Children, &api.TextProperty{
				ID:        fmt.Sprintf("pods.%d", i),
				Reference: ref,
				Value:     pod.Name,
				Widget: func(w gtk.Widgetter, nv *adw.NavigationView) {
					switch row := w.(type) {
					case *adw.ActionRow:
						row.AddPrefix(api.NewStatusWithObject(&pod).Icon())
					}
				},
			})
		}
		props = append(props, podsProp)

		if len(object.Spec.VolumeClaimTemplates) > 0 {
			claimProp := &api.GroupProperty{Name: "Volume Claims"}
			for _, claim := range object.Spec.VolumeClaimTemplates {
				for replica := 0; replica < int(*object.Spec.Replicas); replica++ {
					e.SetObjectGVK(&claim)
					ref := corev1.ObjectReference{
						Kind:       claim.Kind,
						APIVersion: claim.APIVersion,
						Name:       fmt.Sprintf("%s-%s-%d", claim.Name, object.Name, replica),
						Namespace:  object.Namespace,
					}
					pv, _ := e.GetReference(ctx, ref)
					prop := &api.TextProperty{
						Reference: &ref,
						Value:     claim.Name,
						Widget: func(w gtk.Widgetter, nv *adw.NavigationView) {
							switch row := w.(type) {
							case *adw.ActionRow:
								if pv != nil {
									row.AddPrefix(api.NewStatusWithObject(pv).Icon())
								}
							}
						},
					}
					claimProp.Children = append(claimProp.Children, prop)
				}
			}
			props = append(props, claimProp)
		}
	case *appsv1.DaemonSet:
		props = append(props, e.daemonSetActionsProperty(ctx, object))
	}

	return props
}

// workloadActionsProperty returns an Actions group for scalable workloads (Deployment, StatefulSet, ReplicaSet).
// withRestart enables the rolling restart row (not applicable to ReplicaSets).
func (e *Apps) workloadActionsProperty(ctx context.Context, obj client.Object, getReplicas func() int32, withRestart bool) api.Property {
	return &api.GroupProperty{
		ID:       "actions",
		Priority: 100,
		Name:     "Actions",
		Widget: func(w gtk.Widgetter, _ *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}

			// Scale row
			current := getReplicas()
			scaleRow := adw.NewActionRow()
			scaleRow.SetTitle("Replicas")
			scaleRow.SetSubtitle(fmt.Sprintf("%d currently running", current))

			btnBox := gtk.NewBox(gtk.OrientationHorizontal, 0)
			btnBox.AddCSSClass("linked")
			btnBox.SetVAlign(gtk.AlignCenter)

			minus := gtk.NewButtonWithLabel("−")
			minus.SetTooltipText("Scale down by 1")
			minus.SetSensitive(current > 0)

			plus := gtk.NewButtonWithLabel("+")
			plus.SetTooltipText("Scale up by 1")

			minus.ConnectClicked(func() {
				n := getReplicas() - 1
				if n < 0 {
					n = 0
				}
				patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, n)
				if err := e.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
					widget.ShowErrorDialog(ctx, "Failed to scale", err)
				}
			})
			plus.ConnectClicked(func() {
				n := getReplicas() + 1
				patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, n)
				if err := e.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
					widget.ShowErrorDialog(ctx, "Failed to scale", err)
				}
			})

			btnBox.Append(minus)
			btnBox.Append(plus)
			scaleRow.AddSuffix(btnBox)
			group.Add(scaleRow)

			if withRestart {
				restartRow := adw.NewActionRow()
				restartRow.SetTitle("Rolling Restart")
				restartRow.SetSubtitle("Gradually replace all pods with fresh instances")
				restartBtn := gtk.NewButtonWithLabel("Restart")
				restartBtn.AddCSSClass("suggested-action")
				restartBtn.SetVAlign(gtk.AlignCenter)
				restartBtn.ConnectClicked(func() {
					now := time.Now().UTC().Format(time.RFC3339)
					patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, now)
					if err := e.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to trigger rolling restart", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Rolling restart triggered"))
				})
				restartRow.AddSuffix(restartBtn)
				group.Add(restartRow)
			}
		},
	}
}

// daemonSetActionsProperty returns an Actions group for DaemonSets (rolling restart only).
func (e *Apps) daemonSetActionsProperty(ctx context.Context, ds *appsv1.DaemonSet) api.Property {
	return &api.GroupProperty{
		ID:       "actions",
		Priority: 100,
		Name:     "Actions",
		Widget: func(w gtk.Widgetter, _ *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}
			restartRow := adw.NewActionRow()
			restartRow.SetTitle("Rolling Restart")
			restartRow.SetSubtitle("Gradually replace all DaemonSet pods with fresh instances")
			restartBtn := gtk.NewButtonWithLabel("Restart")
			restartBtn.AddCSSClass("suggested-action")
			restartBtn.SetVAlign(gtk.AlignCenter)
			restartBtn.ConnectClicked(func() {
				now := time.Now().UTC().Format(time.RFC3339)
				patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, now)
				if err := e.Patch(ctx, ds, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
					widget.ShowErrorDialog(ctx, "Failed to trigger rolling restart", err)
					return
				}
				ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Rolling restart triggered"))
			})
			restartRow.AddSuffix(restartBtn)
			group.Add(restartRow)
		},
	}
}

