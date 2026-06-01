package extension

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/reference"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	Extensions = append(Extensions, NewBatch)
}

func NewBatch(_ context.Context, cluster *api.Cluster) (Extension, error) {
	return &Batch{Cluster: cluster}, nil
}

type Batch struct {
	Noop
	*api.Cluster
}

func (e *Batch) CreateColumns(ctx context.Context, resource *metav1.APIResource, columns []api.Column) []api.Column {
	switch util.GVRForResource(resource).String() {
	case batchv1.SchemeGroupVersion.WithResource("jobs").String():
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
				Name:     "Completions",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					job := object.(*batchv1.Job)
					cell.SetLabel("%d/%d", job.Status.Succeeded, ptr.Deref(job.Spec.Completions, 1))
				},
			},
		)
	case batchv1.SchemeGroupVersion.WithResource("cronjobs").String():
		columns = append(columns,
			api.Column{
				Name:     "Last schedule",
				Priority: 70,
				Bind: func(cell api.Cell, object client.Object) {
					cron := object.(*batchv1.CronJob)
					if cron.Status.LastScheduleTime != nil {
						duration := time.Since(cron.Status.LastScheduleTime.Time)
						cell.SetLabel(util.HumanizeApproximateDuration(duration))
					}
				},
			},
		)
	}
	return columns
}

func (e *Batch) CreateObjectProperties(ctx context.Context, resource *metav1.APIResource, object client.Object, props []api.Property) []api.Property {
	switch object := object.(type) {
	case *batchv1.Job:
		var images []string
		for _, c := range object.Spec.Template.Spec.Containers {
			images = append(images, c.Image)
		}

		props = append(props, &api.GroupProperty{Name: "Job", Children: []api.Property{
			&api.TextProperty{
				Name:  "Images",
				Value: strings.Join(images, ", "),
			},
			&api.TextProperty{
				Name:  "Completions",
				Value: fmt.Sprintf("%d", ptr.Deref(object.Spec.Completions, 1)),
			},
			&api.TextProperty{
				Name:  "Parallelism",
				Value: fmt.Sprintf("%d", ptr.Deref(object.Spec.Parallelism, 1)),
			},
			&api.TextProperty{
				Name:  "Backoff limit",
				Value: fmt.Sprintf("%d", ptr.Deref(object.Spec.BackoffLimit, 6)),
			},
		}})

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
	case *batchv1.CronJob:
		props = append(props, e.cronJobActionsProperty(ctx, object))
	}

	return props
}

func (e *Batch) cronJobActionsProperty(ctx context.Context, cron *batchv1.CronJob) api.Property {
	suspended := ptr.Deref(cron.Spec.Suspend, false)
	return &api.GroupProperty{
		ID:       "actions",
		Priority: 100,
		Name:     "Actions",
		Widget: func(w gtk.Widgetter, _ *adw.NavigationView) {
			group, ok := w.(*adw.PreferencesGroup)
			if !ok {
				return
			}

			// Trigger now
			triggerRow := adw.NewActionRow()
			triggerRow.SetTitle("Trigger Job")
			triggerRow.SetSubtitle("Create a job run from this CronJob immediately")
			triggerBtn := gtk.NewButtonWithLabel("Run Now")
			triggerBtn.AddCSSClass("suggested-action")
			triggerBtn.SetVAlign(gtk.AlignCenter)
			triggerBtn.ConnectClicked(func() {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:        fmt.Sprintf("%s-manual-%d", cron.Name, time.Now().Unix()),
						Namespace:   cron.Namespace,
						Annotations: map[string]string{"cronjob.kubernetes.io/instantiate": "manual"},
						Labels:      cron.Spec.JobTemplate.Labels,
					},
					Spec: cron.Spec.JobTemplate.Spec,
				}
				if err := e.Create(ctx, job); err != nil {
					widget.ShowErrorDialog(ctx, "Failed to trigger job", err)
					return
				}
				ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast(fmt.Sprintf("Job \"%s\" created", job.Name)))
			})
			triggerRow.AddSuffix(triggerBtn)
			group.Add(triggerRow)

			// Suspend / Resume
			suspendRow := adw.NewActionRow()
			if suspended {
				suspendRow.SetTitle("CronJob is suspended")
				suspendRow.SetSubtitle("No new jobs will be scheduled")
				resumeBtn := gtk.NewButtonWithLabel("Resume")
				resumeBtn.AddCSSClass("suggested-action")
				resumeBtn.SetVAlign(gtk.AlignCenter)
				resumeBtn.ConnectClicked(func() {
					if err := e.Patch(ctx, cron, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":false}}`))); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to resume CronJob", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("CronJob resumed"))
				})
				suspendRow.AddSuffix(resumeBtn)
			} else {
				suspendRow.SetTitle("CronJob is active")
				suspendRow.SetSubtitle("Jobs are scheduled according to the cron expression")
				suspendBtn := gtk.NewButtonWithLabel("Suspend")
				suspendBtn.SetVAlign(gtk.AlignCenter)
				suspendBtn.ConnectClicked(func() {
					if err := e.Patch(ctx, cron, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))); err != nil {
						widget.ShowErrorDialog(ctx, "Failed to suspend CronJob", err)
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast("CronJob suspended"))
				})
				suspendRow.AddSuffix(suspendBtn)
			}
			group.Add(suspendRow)
		},
	}
}

