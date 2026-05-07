package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/style"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/SilkePilon/Orchestrator/widget"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/reference"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	Extensions = append(Extensions, NewCore)
}

func NewCore(_ context.Context, cluster *api.Cluster) (Extension, error) {
	return &Core{
		Cluster:     cluster,
		portforward: PortForwarder{cluster, map[types.NamespacedName]*portforward.PortForwarder{}},
	}, nil
}

type Core struct {
	Noop
	*api.Cluster
	portforward PortForwarder
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
	columns = append(columns, e.quickActionColumn(ctx))
	return columns
}

func (e *Core) CreateObjectProperties(ctx context.Context, apiResource *metav1.APIResource, object client.Object, props []api.Property) []api.Property {
	switch object := object.(type) {
	case *corev1.Pod:
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

		props = append(props, e.quickActionsProperty(ctx, object), &api.GroupProperty{Name: "Containers", Children: containers})
	case *appsv1.Deployment:
		props = append(props, e.quickActionsProperty(ctx, object), e.aggregatedLogsProperty(ctx, "Deployment Logs", object.Namespace, object.Spec.Selector))
	case *appsv1.StatefulSet:
		props = append(props, e.quickActionsProperty(ctx, object), e.aggregatedLogsProperty(ctx, "StatefulSet Logs", object.Namespace, object.Spec.Selector))
	case *appsv1.DaemonSet:
		props = append(props, e.quickActionsProperty(ctx, object), e.aggregatedLogsProperty(ctx, "DaemonSet Logs", object.Namespace, object.Spec.Selector))
	case *corev1.ConfigMap:
		var data []api.Property
		for key, value := range object.Data {
			data = append(data, &api.TextProperty{Name: key, Value: value})
		}
		props = append(props, &api.GroupProperty{Name: "Data", Children: data})
	case *corev1.Secret:
		var data []api.Property
		for key, value := range object.Data {
			data = append(data, &api.TextProperty{Name: key, Value: string(value)})
		}
		props = append(props, &api.GroupProperty{Name: "Data", Children: data})
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

func (e *Core) quickActionColumn(ctx context.Context) api.Column {
	return api.Column{
		Name:     "Actions",
		Priority: -100,
		Bind: func(cell api.Cell, object client.Object) {
			button, ok := e.quickActionButton(ctx, object)
			if !ok {
				return
			}
			cell.SetChild(button)
		},
	}
}

func (e *Core) quickActionsProperty(ctx context.Context, object client.Object) api.Property {
	return &api.GroupProperty{
		Name: "Actions",
		Widget: func(w gtk.Widgetter, nav *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}
			if button, ok := e.quickActionButton(ctx, object); ok {
				group.SetHeaderSuffix(button)
			}
		},
	}
}

func (e *Core) quickActionButton(ctx context.Context, object client.Object) (*gtk.Button, bool) {
	switch object.(type) {
	case *corev1.Pod:
		button := gtk.NewButtonFromIconName("view-refresh-symbolic")
		button.SetTooltipText("Delete pod so its controller can recreate it")
		button.ConnectClicked(func() {
			e.runQuickAction(ctx, "Could not delete pod", func() error {
				return e.Delete(ctx, object)
			})
		})
		return button, true
	case *appsv1.Deployment, *appsv1.StatefulSet, *appsv1.DaemonSet:
		button := gtk.NewButtonFromIconName("view-refresh-symbolic")
		button.SetTooltipText("Restart rollout")
		button.ConnectClicked(func() {
			e.runQuickAction(ctx, "Could not restart rollout", func() error {
				return e.restartWorkload(ctx, object)
			})
		})
		return button, true
	default:
		return nil, false
	}
}

func (e *Core) runQuickAction(ctx context.Context, title string, action func() error) {
	go func() {
		if err := action(); err != nil {
			glib.IdleAdd(func() {
				widget.ShowErrorDialog(ctx, title, err)
			})
		}
	}()
}

func (e *Core) restartWorkload(ctx context.Context, object client.Object) error {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
	object.SetAnnotations(annotations)
	return e.Update(ctx, object)
}
