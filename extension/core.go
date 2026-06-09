package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/style"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/reference"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	Extensions = append(Extensions, NewCore)
}

func NewCore(ctx context.Context, cluster *api.Cluster) (Extension, error) {
	return &Core{
		Cluster:     cluster,
		portforward: NewPortForwarder(ctx, cluster),
	}, nil
}

type Core struct {
	Noop
	*api.Cluster
	portforward *PortForwarder
}

func (e *Core) CreateColumns(ctx context.Context, res *metav1.APIResource, columns []api.Column) []api.Column {
	switch util.GVRForResource(res).String() {
	case corev1.SchemeGroupVersion.WithResource("pods").String():
		columns = append(columns,
			api.Column{
				Name:     "Status",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					box := gtk.NewBox(gtk.OrientationHorizontal, 4)
					for _, icon := range api.NewStatusWithObject(object).Icons() {
						box.Append(icon)
					}
					cell.SetChild(box)
				},
				Compare: api.CompareObjectStatus,
			},
			api.Column{
				Name:     "CPU",
				Priority: 50,
				Bind: func(cell api.Cell, object client.Object) {
					pod := object.(*corev1.Pod)
					req := resource.NewQuantity(0, resource.DecimalSI)
					for _, container := range pod.Spec.Containers {
						if cpu := container.Resources.Requests.Cpu(); cpu != nil {
							req.Add(*cpu)
						}
					}
					_, use := e.Metrics.PodSum(types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
					req.RoundUp(resource.Milli)
					if use != nil {
						use.RoundUp(resource.Milli)
					}
					bar := widget.NewResourceBar(use, req, "")
					bar.SetHAlign(gtk.AlignStart)
					cell.SetChild(bar)
				},
			},
			api.Column{
				Name:     "Memory",
				Priority: 40,
				Bind: func(cell api.Cell, object client.Object) {
					pod := object.(*corev1.Pod)
					req := resource.NewQuantity(0, resource.DecimalSI)
					for _, container := range pod.Spec.Containers {
						if mem := container.Resources.Requests.Memory(); mem != nil {
							req.Add(*mem)
						}
					}
					use, _ := e.Metrics.PodSum(types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
					req.RoundUp(resource.Mega)
					if use != nil {
						use.RoundUp(resource.Mega)
					}
					bar := widget.NewResourceBar(use, req, "")
					bar.SetHAlign(gtk.AlignStart)
					cell.SetChild(bar)
				},
			},
		)
	case corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims").String():
		columns = append(columns,
			api.Column{
				Name:     "Size",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					pvc := object.(*corev1.PersistentVolumeClaim)
					cell.SetLabel(pvc.Spec.Resources.Requests.Storage().String())
				},
			},
			api.Column{
				Name:     "Class",
				Priority: 60,
				Bind: func(cell api.Cell, object client.Object) {
					pvc := object.(*corev1.PersistentVolumeClaim)
					cell.SetLabel(ptr.Deref(pvc.Spec.StorageClassName, ""))
				},
			},
			api.Column{
				Name:     "Volume",
				Priority: 60,
				Bind: func(cell api.Cell, object client.Object) {
					pvc := object.(*corev1.PersistentVolumeClaim)
					cell.SetLabel(pvc.Spec.VolumeName)
				},
			},
		)
	case corev1.SchemeGroupVersion.WithResource("persistentvolumes").String():
		columns = append(columns,
			api.Column{
				Name:     "Size",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					pv := object.(*corev1.PersistentVolume)
					cell.SetLabel(pv.Spec.Capacity.Storage().String())
				},
			},
			api.Column{
				Name:     "Phase",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					pv := object.(*corev1.PersistentVolume)
					cell.SetLabel(string(pv.Status.Phase))
				},
			},
			api.Column{
				Name:     "Claim",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					pv := object.(*corev1.PersistentVolume)
					if pv.Spec.ClaimRef != nil {
						cell.SetLabel(string(pv.Spec.ClaimRef.Name))
					}
				},
			},
		)

	case corev1.SchemeGroupVersion.WithResource("nodes").String():
		columns = append(columns,
			api.Column{
				Name:     "Role",
				Priority: 90,
				Bind: func(cell api.Cell, object client.Object) {
					node := object.(*corev1.Node)
					badge := gtk.NewLabel("")
					badge.AddCSSClass("pill")
					badge.AddCSSClass("caption")
					badge.SetHAlign(gtk.AlignStart)
					badge.SetVAlign(gtk.AlignCenter)
					if IsNodeControlPlane(node) {
						badge.SetText("Control Plane")
						badge.AddCSSClass("success")
					} else {
						badge.SetText("Worker")
						badge.AddCSSClass("accent")
					}
					cell.SetChild(badge)
				},
				Compare: func(a, b client.Object) int {
					na, nb := a.(*corev1.Node), b.(*corev1.Node)
					ca, cb := IsNodeControlPlane(na), IsNodeControlPlane(nb)
					if ca == cb {
						return 0
					}
					if ca {
						return -1
					}
					return 1
				},
			},
			api.Column{
				Name:     "Status",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					cell.SetChild(api.NewStatusWithObject(object).Icon())
				},
				Compare: api.CompareObjectStatus,
			},
			api.Column{
				Name:     "Pods",
				Priority: 60,
				Bind: func(cell api.Cell, object client.Object) {
					pod := object.(*corev1.Node)
					var pods corev1.PodList
					e.List(ctx, &pods, client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.nodeName", pod.Name)})
					cell.SetLabel("%d", len(pods.Items))
				},
			},
			api.Column{
				Name:     "Memory",
				Priority: 50,
				Bind: func(cell api.Cell, object client.Object) {
					node := object.(*corev1.Node)
					metrics := e.Metrics.Node(node.Name)
					if metrics == nil {
						return
					}
					bar := widget.NewResourceBar(metrics.Usage.Memory(), node.Status.Allocatable.Memory(), "")
					bar.SetHAlign(gtk.AlignStart)
					cell.SetChild(bar)
				},
			},
			api.Column{
				Name:     "CPU",
				Priority: 40,
				Bind: func(cell api.Cell, object client.Object) {
					node := object.(*corev1.Node)
					metrics := e.Metrics.Node(node.Name)
					if metrics == nil {
						return
					}
					bar := widget.NewResourceBar(metrics.Usage.Cpu(), node.Status.Allocatable.Cpu(), "")
					bar.SetHAlign(gtk.AlignStart)
					cell.SetChild(bar)
				},
			},
		)
	case corev1.SchemeGroupVersion.WithResource("namespaces").String():
		columns = append(columns,
			api.Column{
				Name:     "Phase",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					ns := object.(*corev1.Namespace)
					cell.SetLabel(string(ns.Status.Phase))
				},
			},
		)
	case corev1.SchemeGroupVersion.WithResource("configmaps").String():
		columns = append(columns,
			api.Column{
				Name:     "Keys",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					configmap := object.(*corev1.ConfigMap)
					var keys []string
					for key := range configmap.Data {
						keys = append(keys, key)
					}
					cell.SetLabel(strings.Join(keys, ", "))
				},
			},
		)
	case corev1.SchemeGroupVersion.WithResource("secrets").String():
		columns = append(columns,
			api.Column{
				Name:     "Keys",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					secret := object.(*corev1.Secret)
					var keys []string
					for key := range secret.Data {
						keys = append(keys, key)
					}
					cell.SetLabel(strings.Join(keys, ", "))
				},
			},
		)
	case corev1.SchemeGroupVersion.WithResource("services").String():
		columns = append(columns,
			api.Column{
				Name:     "Type",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					service := object.(*corev1.Service)
					cell.SetLabel(string(service.Spec.Type))
				},
			},
			api.Column{
				Name:     "Ports",
				Priority: 60,
				Bind: func(cell api.Cell, object client.Object) {
					svc := object.(*corev1.Service)
					var ports []string
					for _, port := range svc.Spec.Ports {
						ports = append(ports, strconv.Itoa(int(port.Port)))
					}
					cell.SetLabel(strings.Join(ports, ", "))
				},
			},
		)
	}
	return columns
}

func (e *Core) CreateObjectProperties(ctx context.Context, apiResource *metav1.APIResource, object client.Object, props []api.Property) []api.Property {
	switch object := object.(type) {
	case *corev1.Pod:
		props = append(props, e.podActionsProperty(ctx, object))
		var containers []api.Property

		for i, container := range object.Spec.Containers {
			var props []api.Property
			var status corev1.ContainerStatus
			for _, s := range object.Status.ContainerStatuses {
				if s.Name == container.Name {
					status = s
					break
				}
			}

			podMetrics := e.Metrics.Pod(types.NamespacedName{Name: object.Name, Namespace: object.Namespace})
			var metrics *metricsv1beta1.ContainerMetrics
			if podMetrics != nil {
				for _, m := range podMetrics.Containers {
					if m.Name == container.Name {
						metrics = &m
						break
					}
				}
			}

			var state string
			if status.State.Running != nil {
				state = "Running"
			} else if status.State.Terminated != nil {
				message := status.State.Terminated.Message
				if len(message) == 0 {
					message = status.State.Terminated.Reason
				}
				state = fmt.Sprintf("Terminated: %s", message)
			} else if status.State.Waiting != nil {
				message := status.State.Waiting.Message
				if len(message) == 0 {
					message = status.State.Waiting.Reason
				}
				state = fmt.Sprintf("Waiting: %s", message)
			}
			props = append(props, &api.TextProperty{Name: "State", Value: state})

			var restartCount int
			for _, container := range object.Status.ContainerStatuses {
				restartCount += int(container.RestartCount)
			}
			props = append(props, &api.TextProperty{Name: "Restarts", Value: fmt.Sprintf("%d", restartCount)})

			props = append(props, &api.TextProperty{Name: "Image", Value: container.Image})

			if len(container.Command) > 0 {
				props = append(props, &api.TextProperty{Name: "Command", Value: strings.Join(container.Command, " ")})
			}

			envs := &api.GroupProperty{Name: "Env"}
			for i, env := range container.Env {
				id := fmt.Sprintf("env.%d", i)
				if from := env.ValueFrom; from != nil {
					if ref := from.ConfigMapKeyRef; ref != nil {
						var cm corev1.ConfigMap
						if err := e.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: object.Namespace}, &cm); err != nil {
							envs.Children = append(envs.Children, &api.TextProperty{ID: id, Name: env.Name, Value: fmt.Sprintf("error: %v", err)})
						} else {
							envs.Children = append(envs.Children, &api.TextProperty{ID: id, Name: env.Name, Value: cm.Data[ref.Key]})
						}
					} else if ref := from.SecretKeyRef; ref != nil {
						var secret corev1.Secret
						if err := e.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: object.Namespace}, &secret); err != nil {
							envs.Children = append(envs.Children, &api.TextProperty{ID: id, Name: env.Name, Value: fmt.Sprintf("error: %v", err)})
						} else {
							envs.Children = append(envs.Children, &api.TextProperty{ID: id, Name: env.Name, Value: string(secret.Data[ref.Key])})
						}
					}
					// TODO field refs
				} else {
					envs.Children = append(envs.Children, &api.TextProperty{ID: id, Name: env.Name, Value: env.Value})
				}
			}
			props = append(props, envs)

			ports := &api.GroupProperty{Name: "Ports"}
			for _, port := range container.Ports {
				ports.Children = append(ports.Children, &api.TextProperty{
					Name:  port.Name,
					Value: fmt.Sprintf("%d", port.ContainerPort),
					Widget: func(w gtk.Widgetter, nv *adw.NavigationView) {
						name := types.NamespacedName{Name: object.Name, Namespace: object.Namespace}
						switch box := w.(type) {
						case *gtk.Box:
							btn := gtk.NewButton()
							e.portforward.UpdateButton(ctx, btn, name, []string{fmt.Sprintf(":%d", port.ContainerPort)})
							box.Append(btn)
						}
					},
				})
			}
			props = append(props, ports)

			cpu := &api.GroupProperty{
				Name: "CPU",
			}
			if metrics != nil {
				if current := metrics.Usage.Cpu(); current != nil {
					current.RoundUp(resource.Milli)
					current.Format = resource.DecimalSI
					cpu.Children = append(cpu.Children, &api.TextProperty{Name: "Current", Value: current.String()})
				}
			}
			cpu.Children = append(cpu.Children, &api.TextProperty{Name: "Request", Value: container.Resources.Requests.Cpu().String()})
			cpu.Children = append(cpu.Children, &api.TextProperty{Name: "Limit", Value: container.Resources.Limits.Cpu().String()})
			props = append(props, cpu)

			mem := &api.GroupProperty{
				Name: "Memory",
			}
			if metrics != nil {
				if current := metrics.Usage.Memory(); current != nil {
					current.RoundUp(resource.Mega)
					current.Format = resource.DecimalSI
					mem.Children = append(mem.Children, &api.TextProperty{Name: "Current", Value: current.String()})
				}
			}
			mem.Children = append(mem.Children, &api.TextProperty{Name: "Request", Value: container.Resources.Requests.Memory().String()})
			mem.Children = append(mem.Children, &api.TextProperty{Name: "Limit", Value: container.Resources.Limits.Memory().String()})
			props = append(props, mem)

			containers = append(containers, &api.GroupProperty{
				ID:   fmt.Sprintf("containers.%d", i),
				Name: container.Name, Children: props,
				Widget: func(w gtk.Widgetter, nav *adw.NavigationView) {
					switch row := w.(type) {
					case *adw.ExpanderRow:
						row.AddPrefix(api.NewStatusWithObject(object).Icon())
						if metrics != nil {
							if mem := metrics.Usage.Memory(); mem != nil {
								req := container.Resources.Requests.Memory()
								if req == nil || req.IsZero() {
									req = container.Resources.Limits.Memory()
								}
								row.AddSuffix(widget.NewResourceBar(mem, req, "memory-symbolic"))
							}
							if cpu := metrics.Usage.Cpu(); cpu != nil {
								req := container.Resources.Requests.Cpu()
								if req == nil || req.IsZero() {
									req = container.Resources.Limits.Cpu()
								}
								row.AddSuffix(widget.NewResourceBar(cpu, req, "processor-symbolic"))
							}
						}

						logs := adw.NewActionRow()
						logs.SetActivatable(true)
						logs.AddSuffix(gtk.NewImageFromIconName("go-next-symbolic"))
						logs.SetTitle("Logs")
						logs.ConnectActivated(func() {
							nav.Push(widget.NewLogPage(ctx, e.Cluster, object, container.Name).NavigationPage)
						})
						row.AddRow(logs)

						if !style.Eq(style.Windows) {
							exec := adw.NewActionRow()
							exec.SetActivatable(true)
							exec.AddSuffix(gtk.NewImageFromIconName("go-next-symbolic"))
							exec.SetTitle("Exec")
							exec.ConnectActivated(func() {
								nav.Push(widget.NewTerminalPage(ctx, e.Cluster, object, container.Name).NavigationPage)
							})
							row.AddRow(exec)
						}
					}
				},
			})
		}

		props = append(props, &api.GroupProperty{Name: "Containers", Children: containers})
	case *appsv1.Deployment:
		props = append(props, e.aggregatedLogsProperty(ctx, "Deployment Logs", object.Namespace, object.Spec.Selector))
	case *appsv1.StatefulSet:
		props = append(props, e.aggregatedLogsProperty(ctx, "StatefulSet Logs", object.Namespace, object.Spec.Selector))
	case *appsv1.DaemonSet:
		props = append(props, e.aggregatedLogsProperty(ctx, "DaemonSet Logs", object.Namespace, object.Spec.Selector))
	case *corev1.ConfigMap:
		readOnly := e.ClusterPreferences.Value().ReadOnly
		var data []api.Property
		for key, value := range object.Data {
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
					editBtn.SetTooltipText("Edit value")
					editBtn.ConnectClicked(func() {
						showEditDialog(ctx, "Edit "+key, value, func(newVal string) {
							patchData, _ := json.Marshal(map[string]any{"data": map[string]any{key: newVal}})
							if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
								widget.ShowErrorDialog(ctx, "Failed to update key", err)
								return
							}
							ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Key updated"))
						})
					})
					row.AddSuffix(editBtn)

					delBtn := gtk.NewButtonFromIconName("edit-delete-symbolic")
					delBtn.AddCSSClass("flat")
					delBtn.AddCSSClass("error")
					delBtn.SetVAlign(gtk.AlignCenter)
					delBtn.SetTooltipText("Delete key")
					delBtn.ConnectClicked(func() {
						dialog := adw.NewAlertDialog("Delete Key?", fmt.Sprintf("Remove \"  %s\" from %s?", key, object.Name))
						dialog.AddResponse("cancel", "Cancel")
						dialog.AddResponse("delete", "Delete")
						dialog.SetResponseAppearance("delete", adw.ResponseDestructive)
						dialog.ConnectResponse(func(r string) {
							if r != "delete" {
								return
							}
							patchData, _ := json.Marshal(map[string]any{"data": map[string]any{key: nil}})
							if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
								widget.ShowErrorDialog(ctx, "Failed to delete key", err)
								return
							}
							ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Key deleted"))
						})
						dialog.Present(ctxt.MustFrom[*gtk.Window](ctx))
					})
					row.AddSuffix(delBtn)
				}
			}
			data = append(data, prop)
		}
		dataGroup := &api.GroupProperty{Name: "Data", Children: data}
		if !readOnly {
			dataGroup.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
				g, ok := w.(*adw.PreferencesGroup)
				if !ok {
					return
				}
				addBtn := gtk.NewButtonWithLabel("Add Key")
				addBtn.AddCSSClass("flat")
				addBtn.ConnectClicked(func() {
					showAddPairDialog(ctx, "Add Key", "key", "value", func(k, v string) {
						patchData, _ := json.Marshal(map[string]any{"data": map[string]any{k: v}})
						if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
							widget.ShowErrorDialog(ctx, "Failed to add key", err)
							return
						}
						ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Key added"))
					})
				})
				g.SetHeaderSuffix(addBtn)
			}
		}
		props = append(props, dataGroup)
	case *corev1.Secret:
		readOnly := e.ClusterPreferences.Value().ReadOnly
		var data []api.Property
		for key, value := range object.Data {
			key, value := key, value
			displayVal := string(value)
			prop := &api.TextProperty{Name: key, Value: displayVal}
			if !readOnly {
				prop.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
					row, ok := w.(*adw.ActionRow)
					if !ok {
						return
					}
					editBtn := gtk.NewButtonFromIconName("edit-symbolic")
					editBtn.AddCSSClass("flat")
					editBtn.SetVAlign(gtk.AlignCenter)
					editBtn.SetTooltipText("Edit value")
					editBtn.ConnectClicked(func() {
						showEditDialog(ctx, "Edit "+key, displayVal, func(newVal string) {
							// json.Marshal encodes []byte as base64 automatically
							patchData, _ := json.Marshal(map[string]any{"data": map[string]any{key: []byte(newVal)}})
							if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
								widget.ShowErrorDialog(ctx, "Failed to update secret key", err)
								return
							}
							ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Secret key updated"))
						})
					})
					row.AddSuffix(editBtn)

					delBtn := gtk.NewButtonFromIconName("edit-delete-symbolic")
					delBtn.AddCSSClass("flat")
					delBtn.AddCSSClass("error")
					delBtn.SetVAlign(gtk.AlignCenter)
					delBtn.SetTooltipText("Delete key")
					delBtn.ConnectClicked(func() {
						dialog := adw.NewAlertDialog("Delete Key?", fmt.Sprintf("Remove \"%s\" from %s?", key, object.Name))
						dialog.AddResponse("cancel", "Cancel")
						dialog.AddResponse("delete", "Delete")
						dialog.SetResponseAppearance("delete", adw.ResponseDestructive)
						dialog.ConnectResponse(func(r string) {
							if r != "delete" {
								return
							}
							patchData, _ := json.Marshal(map[string]any{"data": map[string]any{key: nil}})
							if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
								widget.ShowErrorDialog(ctx, "Failed to delete secret key", err)
								return
							}
							ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Secret key deleted"))
						})
						dialog.Present(ctxt.MustFrom[*gtk.Window](ctx))
					})
					row.AddSuffix(delBtn)
				}
			}
			data = append(data, prop)
		}
		dataGroup := &api.GroupProperty{Name: "Data", Children: data}
		if !readOnly {
			dataGroup.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
				g, ok := w.(*adw.PreferencesGroup)
				if !ok {
					return
				}
				addBtn := gtk.NewButtonWithLabel("Add Key")
				addBtn.AddCSSClass("flat")
				addBtn.ConnectClicked(func() {
					showAddPairDialog(ctx, "Add Secret Key", "key", "value", func(k, v string) {
						patchData, _ := json.Marshal(map[string]any{"data": map[string]any{k: []byte(v)}})
						if err := e.Patch(ctx, object, client.RawPatch(types.MergePatchType, patchData)); err != nil {
							widget.ShowErrorDialog(ctx, "Failed to add secret key", err)
							return
						}
						ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Secret key added"))
					})
				})
				g.SetHeaderSuffix(addBtn)
			}
		}
		props = append(props, dataGroup)
	case *corev1.Service:
		var ports []api.Property
		for _, p := range object.Spec.Ports {
			ports = append(ports, &api.TextProperty{Name: p.Name, Value: strconv.Itoa(int(p.Port))})
		}
		props = append(props, &api.GroupProperty{Name: "Service", Children: []api.Property{
			&api.TextProperty{Name: "Cluster IP", Value: object.Spec.ClusterIP},
			&api.GroupProperty{Name: "Ports", Children: ports},
		}})
	case *corev1.PersistentVolumeClaim:
		var accessModes []string
		for _, m := range object.Spec.AccessModes {
			accessModes = append(accessModes, string(m))
		}
		var storageClass string
		if object.Spec.StorageClassName != nil {
			storageClass = *object.Spec.StorageClassName
		}
		props = append(props, &api.GroupProperty{Name: "Persistent Volume Claim", Children: []api.Property{
			&api.TextProperty{Name: "Class", Value: storageClass},
			&api.TextProperty{Name: "Request", Value: object.Spec.Resources.Requests.Storage().String()},
			&api.TextProperty{Name: "Access modes", Value: strings.Join(accessModes, ", ")},
			&api.TextProperty{
				Name:  "Volume",
				Value: object.Spec.VolumeName,
				Reference: &corev1.ObjectReference{
					Kind:       "PersistentVolume",
					APIVersion: corev1.SchemeGroupVersion.String(),
					Name:       object.Spec.VolumeName,
					Namespace:  object.Namespace,
				},
			},
		}})
	case *corev1.PersistentVolume:
		var accessModes []string
		for _, m := range object.Spec.AccessModes {
			accessModes = append(accessModes, string(m))
		}

		group := &api.GroupProperty{Name: "Persistent Volume", Children: []api.Property{
			&api.TextProperty{Name: "Class", Value: object.Spec.StorageClassName},
			&api.TextProperty{Name: "Capacity", Value: object.Spec.Capacity.Storage().String()},
			&api.TextProperty{Name: "Access modes", Value: strings.Join(accessModes, ", ")},
			&api.TextProperty{Name: "Reclaim policy", Value: string(object.Spec.PersistentVolumeReclaimPolicy)},
			&api.TextProperty{Name: "Phase", Value: string(object.Status.Phase)},
		}}
		if object.Spec.ClaimRef != nil {
			group.Children = append(group.Children, &api.TextProperty{Name: "Claim", Value: object.Spec.ClaimRef.Name, Reference: object.Spec.ClaimRef})
		}
		props = append(props, group)

	case *corev1.Node:
		props = append(props, e.nodeActionsProperty(ctx, object))
		infoProp := &api.GroupProperty{Name: "Info"}
		mem := object.Status.Allocatable.Memory()
		mem.RoundUp(resource.Mega)
		mem.Format = resource.DecimalSI
		cpu := object.Status.Allocatable.Cpu()
		cpu.RoundUp(resource.Milli)
		cpu.Format = resource.DecimalSI
		infoProp.Children = append(infoProp.Children,
			&api.TextProperty{
				Name:  "Architecture",
				Value: object.Status.NodeInfo.Architecture,
			},
			&api.TextProperty{
				Name:  "Container runtime",
				Value: object.Status.NodeInfo.ContainerRuntimeVersion,
			},
			&api.TextProperty{
				Name:  "Kernel",
				Value: object.Status.NodeInfo.KernelVersion,
			},
			&api.TextProperty{
				Name:  "Kubelet",
				Value: object.Status.NodeInfo.KubeletVersion,
			},
			&api.TextProperty{
				Name:  "Operating system image",
				Value: object.Status.NodeInfo.OSImage,
			},
			&api.TextProperty{
				Name:  "Memory",
				Value: fmt.Sprintf("%v", mem),
			},
			&api.TextProperty{
				Name:  "CPU",
				Value: fmt.Sprintf("%v", cpu),
			},
		)
		props = append(props, infoProp)

		props = append(props, e.nodeTaintsProperty(ctx, object))

		podsProp := &api.GroupProperty{Name: "Pods"}
		var pods corev1.PodList
		e.List(ctx, &pods, client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.nodeName", object.Name)})
		for i, pod := range pods.Items {
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
	}

	return append(props, e.resourceDiffProperty(ctx, apiResource, object))
}

func (e *Core) podActionsProperty(ctx context.Context, pod *corev1.Pod) api.Property {
	hasController := len(pod.OwnerReferences) > 0
	return &api.GroupProperty{
		ID:       "actions",
		Priority: 100,
		Name:     "Actions",
		Widget: func(w gtk.Widgetter, _ *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}
			row := adw.NewActionRow()
			row.SetTitle("Recreate Pod")
			if hasController {
				row.SetSubtitle("Delete this pod — its controller will create a replacement")
			} else {
				row.SetSubtitle("Permanently delete this pod")
			}
			btn := gtk.NewButtonWithLabel("Recreate")
			btn.AddCSSClass("destructive-action")
			btn.SetVAlign(gtk.AlignCenter)
			btn.ConnectClicked(func() {
				body := fmt.Sprintf("Delete pod \"%s\"?", pod.Name)
				if hasController {
					body = fmt.Sprintf("Delete pod \"%s\"? Its controller will create a new one.", pod.Name)
				}
				dialog := adw.NewAlertDialog("Recreate Pod?", body)
				dialog.AddResponse("cancel", "Cancel")
				dialog.AddResponse("recreate", "Delete & Recreate")
				dialog.SetResponseAppearance("recreate", adw.ResponseDestructive)
				dialog.ConnectResponse(func(response string) {
					if response != "recreate" {
						return
					}
					if err := e.Delete(ctx, pod); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to delete pod", err)
					}
				})
				dialog.Present(ctxt.MustFrom[*gtk.Window](ctx))
			})
			row.AddSuffix(btn)
			group.Add(row)
		},
	}
}

func (e *Core) nodeActionsProperty(ctx context.Context, node *corev1.Node) api.Property {
	return &api.GroupProperty{
		ID:       "actions",
		Priority: 100,
		Name:     "Actions",
		Widget: func(w gtk.Widgetter, _ *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}

			// Cordon / Uncordon
			cordonRow := adw.NewActionRow()
			if node.Spec.Unschedulable {
				cordonRow.SetTitle("Node is cordoned")
				cordonRow.SetSubtitle("New pods cannot be scheduled on this node")
				btn := gtk.NewButtonWithLabel("Uncordon")
				btn.AddCSSClass("suggested-action")
				btn.SetVAlign(gtk.AlignCenter)
				btn.ConnectClicked(func() {
					if err := e.Patch(ctx, node, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"unschedulable":false}}`))); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to uncordon node", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Node uncordoned"))
				})
				cordonRow.AddSuffix(btn)
			} else {
				cordonRow.SetTitle("Scheduling enabled")
				cordonRow.SetSubtitle("New pods can be scheduled on this node")
				btn := gtk.NewButtonWithLabel("Cordon")
				btn.SetVAlign(gtk.AlignCenter)
				btn.ConnectClicked(func() {
					if err := e.Patch(ctx, node, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`))); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to cordon node", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Node cordoned"))
				})
				cordonRow.AddSuffix(btn)
			}
			group.Add(cordonRow)

			// Drain
			drainRow := adw.NewActionRow()
			drainRow.SetTitle("Drain Node")
			drainRow.SetSubtitle("Cordon and evict all non-DaemonSet pods")
			drainBtn := gtk.NewButtonWithLabel("Drain")
			drainBtn.AddCSSClass("destructive-action")
			drainBtn.SetVAlign(gtk.AlignCenter)
			drainBtn.ConnectClicked(func() {
				dialog := adw.NewAlertDialog(
					"Drain node?",
					fmt.Sprintf("All pods on \"%s\" will be evicted and the node will be cordoned.", node.Name),
				)
				dialog.AddResponse("cancel", "Cancel")
				dialog.AddResponse("drain", "Drain")
				dialog.SetResponseAppearance("drain", adw.ResponseDestructive)
				dialog.ConnectResponse(func(response string) {
					if response != "drain" {
						return
					}
					if err := e.Patch(ctx, node, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`))); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to cordon node during drain", err)
						return
					}
					var pods corev1.PodList
					e.List(ctx, &pods, client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.nodeName", node.Name)})
					var failed int
					for i := range pods.Items {
						pod := &pods.Items[i]
						isDaemonSet := false
						for _, owner := range pod.OwnerReferences {
							if owner.Kind == "DaemonSet" {
								isDaemonSet = true
								break
							}
						}
						if !isDaemonSet {
							if err := e.Delete(ctx, pod); err != nil {
								failed++
							}
						}
					}
					if failed > 0 {
						ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast(fmt.Sprintf("Drain complete (%d pods failed to evict)", failed)))
					} else {
						ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Node drained successfully"))
					}
				})
				dialog.Present(ctxt.MustFrom[*gtk.Window](ctx))
			})
			drainRow.AddSuffix(drainBtn)
			group.Add(drainRow)
		},
	}
}

func (e *Core) nodeTaintsProperty(ctx context.Context, node *corev1.Node) api.Property {
	readOnly := e.ClusterPreferences.Value().ReadOnly
	var taintProps []api.Property
	for i, taint := range node.Spec.Taints {
		i, taint := i, taint
		label := taint.Key
		if taint.Value != "" {
			label = taint.Key + "=" + taint.Value
		}
		prop := &api.TextProperty{Name: label, Value: string(taint.Effect)}
		if !readOnly {
			prop.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
				row, ok := w.(*adw.ActionRow)
				if !ok {
					return
				}
				delBtn := gtk.NewButtonFromIconName("edit-delete-symbolic")
				delBtn.AddCSSClass("flat")
				delBtn.AddCSSClass("error")
				delBtn.SetVAlign(gtk.AlignCenter)
				delBtn.SetTooltipText("Remove taint")
				delBtn.ConnectClicked(func() {
					var newTaints []corev1.Taint
					for j, t := range node.Spec.Taints {
						if j != i {
							newTaints = append(newTaints, t)
						}
					}
					patchData, _ := json.Marshal(map[string]any{
						"spec": map[string]any{"taints": newTaints},
					})
					if err := e.Patch(ctx, node, client.RawPatch(types.MergePatchType, patchData)); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to remove taint", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Taint removed"))
				})
				row.AddSuffix(delBtn)
			}
		}
		taintProps = append(taintProps, prop)
	}

	taintsGroup := &api.GroupProperty{Name: "Taints", Children: taintProps}
	if !readOnly {
		taintsGroup.Widget = func(w gtk.Widgetter, _ *adw.NavigationView) {
			g, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}
			addBtn := gtk.NewButtonWithLabel("Add Taint")
			addBtn.AddCSSClass("flat")
			addBtn.ConnectClicked(func() {
				showAddTaintDialog(ctx, func(key, value string, effect corev1.TaintEffect) {
					newTaints := append(node.Spec.Taints, corev1.Taint{Key: key, Value: value, Effect: effect})
					patchData, _ := json.Marshal(map[string]any{
						"spec": map[string]any{"taints": newTaints},
					})
					if err := e.Patch(ctx, node, client.RawPatch(types.MergePatchType, patchData)); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to add taint", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("Taint added"))
				})
			})
			g.SetHeaderSuffix(addBtn)
		}
	}
	return taintsGroup
}

func showAddTaintDialog(ctx context.Context, onAdd func(key, value string, effect corev1.TaintEffect)) {
	effects := []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

	dialog := adw.NewAlertDialog("Add Taint", "")

	keyEntry := gtk.NewEntry()
	keyEntry.SetPlaceholderText("Key (e.g. dedicated)")

	valueEntry := gtk.NewEntry()
	valueEntry.SetPlaceholderText("Value (optional)")

	effectDrop := gtk.NewDropDownFromStrings(effects)

	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.SetMarginTop(4)
	box.Append(keyEntry)
	box.Append(valueEntry)
	box.Append(effectDrop)
	dialog.SetExtraChild(box)

	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("add", "Add Taint")
	dialog.SetDefaultResponse("add")
	dialog.SetResponseAppearance("add", adw.ResponseSuggested)
	dialog.ConnectResponse(func(response string) {
		if response == "add" && keyEntry.Text() != "" {
			onAdd(keyEntry.Text(), valueEntry.Text(), corev1.TaintEffect(effects[effectDrop.Selected()]))
		}
	})
	dialog.Present(ctxt.MustFrom[*gtk.Window](ctx))
}

func (e *Core) aggregatedLogsProperty(ctx context.Context, title, namespace string, selector *metav1.LabelSelector) api.Property {
	return &api.GroupProperty{
		Name: title,
		Widget: func(w gtk.Widgetter, nav *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}
			button := gtk.NewButtonWithLabel("Open")
			button.AddCSSClass("flat")
			button.SetTooltipText("Open aggregated logs for matching pods")
			button.ConnectClicked(func() {
				nav.Push(widget.NewAggregatedLogPage(ctx, e.Cluster, title, namespace, selector).NavigationPage)
			})
			group.SetHeaderSuffix(button)
		},
	}
}

func (e *Core) resourceDiffProperty(ctx context.Context, apiResource *metav1.APIResource, object client.Object) api.Property {
	return &api.GroupProperty{
		Name: "Resource Diff",
		Widget: func(w gtk.Widgetter, nav *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}
			button := gtk.NewButtonWithLabel("Diff")
			button.AddCSSClass("flat")
			button.SetTooltipText("Compare this resource with the last local snapshot")
			button.ConnectClicked(func() {
				page, err := e.newResourceDiffPage(apiResource, object)
				if err != nil {
					widget.ShowErrorDialog(ctx, "Could not open resource diff", err)
					return
				}
				nav.Push(page)
			})
			group.SetHeaderSuffix(button)
		},
	}
}

func (e *Core) newResourceDiffPage(apiResource *metav1.APIResource, object client.Object) (*adw.NavigationPage, error) {
	current, err := resourceSnapshot(object)
	if err != nil {
		return nil, err
	}
	path, err := e.resourceSnapshotPath(apiResource, object)
	if err != nil {
		return nil, err
	}
	previous, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, current, 0o600); err != nil {
		return nil, err
	}

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("view")
	header := adw.NewHeaderBar()
	header.SetShowStartTitleButtons(false)
	header.AddCSSClass("flat")
	box.Append(header)

	view := gtk.NewTextView()
	view.SetEditable(false)
	view.SetCursorVisible(false)
	view.SetMonospace(true)
	view.SetWrapMode(gtk.WrapNone)
	view.Buffer().Insert(view.Buffer().EndIter(), resourceSnapshotDiff(previous, current))

	scroll := gtk.NewScrolledWindow()
	scroll.SetHExpand(true)
	scroll.SetVExpand(true)
	scroll.SetChild(view)
	box.Append(scroll)

	return adw.NewNavigationPage(box, fmt.Sprintf("Diff %s", object.GetName())), nil
}

func (e *Core) resourceSnapshotPath(apiResource *metav1.APIResource, object client.Object) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	cluster := safeResourceSnapshotName(e.ClusterPreferences.Value().Name)
	resourceName := safeResourceSnapshotName(util.GVRForResource(apiResource).String())
	objectName := safeResourceSnapshotName(object.GetName())
	if namespace := object.GetNamespace(); namespace != "" {
		objectName = safeResourceSnapshotName(namespace) + "-" + objectName
	}
	return filepath.Join(configDir, "orchestrator", "resource-diff", cluster, resourceName, objectName+".json"), nil
}

func resourceSnapshot(object client.Object) ([]byte, error) {
	data, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	var snapshot map[string]any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	if metadata, ok := snapshot["metadata"].(map[string]any); ok {
		delete(metadata, "managedFields")
		delete(metadata, "resourceVersion")
		delete(metadata, "uid")
		delete(metadata, "selfLink")
	}
	if strings.EqualFold(fmt.Sprint(snapshot["kind"]), "Secret") {
		redactStringMap(snapshot, "data")
		redactStringMap(snapshot, "stringData")
	}
	return json.MarshalIndent(snapshot, "", "  ")
}

func redactStringMap(snapshot map[string]any, key string) {
	values, ok := snapshot[key].(map[string]any)
	if !ok {
		return
	}
	for name := range values {
		values[name] = "<redacted>"
	}
}

func resourceSnapshotDiff(previous, current []byte) string {
	if len(previous) == 0 {
		return "No previous snapshot was available.\n\nThe current resource snapshot has been saved; open Resource Diff again after the resource changes to compare it.\n\n" + string(current)
	}
	if string(previous) == string(current) {
		return "No resource changes since the last local snapshot.\n\n" + string(current)
	}
	previousLines := strings.Split(string(previous), "\n")
	currentLines := strings.Split(string(current), "\n")
	var diff strings.Builder
	diff.WriteString("--- previous snapshot\n+++ current resource\n")
	lineCount := max(len(previousLines), len(currentLines))
	for i := 0; i < lineCount; i++ {
		var before, after string
		if i < len(previousLines) {
			before = previousLines[i]
		}
		if i < len(currentLines) {
			after = currentLines[i]
		}
		if before == after {
			diff.WriteString("  " + before + "\n")
			continue
		}
		if i < len(previousLines) {
			diff.WriteString("- " + before + "\n")
		}
		if i < len(currentLines) {
			diff.WriteString("+ " + after + "\n")
		}
	}
	return diff.String()
}

func safeResourceSnapshotName(value string) string {
	value = strings.ToLower(value)
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, value), "-")
}
