// Package notifications watches cluster resources and sends desktop
// notifications based on per-cluster NotificationPreferences.
package notifications

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	gitopsbe "github.com/SilkePilon/Orchestrator/internal/gitops"
	"github.com/SilkePilon/Orchestrator/internal/pubsub"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cooldownTracker suppresses repeated notifications for the same event within
// a configurable time window.
type cooldownTracker struct {
	mu       sync.Mutex
	lastSent map[string]time.Time
}

func newCooldownTracker() *cooldownTracker {
	return &cooldownTracker{lastSent: map[string]time.Time{}}
}

// allow returns true if the caller may send a notification with the given id.
// A duration of 0 disables suppression.
func (c *cooldownTracker) allow(id string, duration time.Duration) bool {
	if duration <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.lastSent[id]; ok && time.Since(last) < duration {
		return false
	}
	c.lastSent[id] = time.Now()
	return true
}

// Dispatcher subscribes to cluster resource changes and sends desktop
// notifications based on the cluster's NotificationPreferences.
type Dispatcher struct {
	app          *gtk.Application
	clusterPrefs pubsub.Property[api.ClusterPreferences]
	cooldown     *cooldownTracker
}

// New creates a Dispatcher. Call Start to begin watching the cluster.
func New(app *gtk.Application, clusterPrefs pubsub.Property[api.ClusterPreferences]) *Dispatcher {
	return &Dispatcher{
		app:          app,
		clusterPrefs: clusterPrefs,
		cooldown:     newCooldownTracker(),
	}
}

// Start subscribes to all watched resource types. It stops automatically when
// ctx is cancelled.
func (d *Dispatcher) Start(ctx context.Context, cluster *api.Cluster) {
	d.watchPods(ctx, cluster)
	d.watchDeployments(ctx, cluster)
	d.watchStatefulSets(ctx, cluster)
	d.watchDaemonSets(ctx, cluster)
	d.watchNodes(ctx, cluster)
	d.watchJobs(ctx, cluster)
	d.watchPVCs(ctx, cluster)
	d.watchConfigMaps(ctx, cluster)
	d.watchSecrets(ctx, cluster)
	d.watchNamespaces(ctx, cluster)
	d.watchIngresses(ctx, cluster)
	d.watchArgo(ctx, cluster)
	d.watchFluxGVR(ctx, cluster, gitopsbe.GVRKustomization, "Kustomization",
		func(p api.NotificationPreferences) bool { return p.FluxKustomizationFailed })
	d.watchFluxGVR(ctx, cluster, gitopsbe.GVRHelmRelease, "HelmRelease",
		func(p api.NotificationPreferences) bool { return p.FluxHelmReleaseFailed })
	d.watchFluxSource(ctx, cluster, gitopsbe.GVRGitRepository, "GitRepository")
	d.watchFluxSource(ctx, cluster, gitopsbe.GVRHelmRepository, "HelmRepository")
}

func (d *Dispatcher) prefs() api.NotificationPreferences {
	return d.clusterPrefs.Value().Notifications
}

func (d *Dispatcher) clusterName() string {
	return d.clusterPrefs.Value().Name
}

func (d *Dispatcher) excluded(namespace, name string) bool {
	p := d.prefs()
	for _, ns := range p.ExcludedNamespaces {
		if ns == namespace {
			return true
		}
	}
	key := namespace + "/" + name
	for _, exc := range p.ExcludedResources {
		if exc == key || exc == name {
			return true
		}
	}
	return false
}

func (d *Dispatcher) notify(id, title, body string) {
	p := d.prefs()
	dur := time.Duration(p.CooldownSeconds) * time.Second
	if !d.cooldown.allow(id, dur) {
		return
	}
	notif := gio.NewNotification(title)
	notif.SetBody(body)
	notif.SetDefaultAction("app.activate")
	d.app.SendNotification(id, notif)
}

// ─── Pod watcher ────────────────────────────────────────────────────────────

type podTracked struct {
	failReason   string
	restartCount int32
}

func (d *Dispatcher) watchPods(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*corev1.Pod{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Version: "v1", Resource: "pods"}, prop); err != nil {
		return
	}
	known := map[types.UID]podTracked{}
	var seeded bool
	prop.Sub(ctx, func(pods []*corev1.Pod) {
		defer func() { seeded = true }()
		for _, pod := range pods {
			reason := podFailureReason(pod)
			restarts := podTotalRestarts(pod)
			if !seeded {
				known[pod.UID] = podTracked{failReason: reason, restartCount: restarts}
				continue
			}
			p := d.prefs()
			if !p.Enabled || d.excluded(pod.Namespace, pod.Name) {
				known[pod.UID] = podTracked{failReason: reason, restartCount: restarts}
				continue
			}
			prev, seen := known[pod.UID]
			known[pod.UID] = podTracked{failReason: reason, restartCount: restarts}

			// New pod
			if !seen {
				if p.NewPod && pod.DeletionTimestamp == nil {
					d.notify(
						fmt.Sprintf("pod-new-%s-%s", pod.Namespace, pod.Name),
						fmt.Sprintf("New pod — %s", d.clusterName()),
						fmt.Sprintf("%s/%s scheduled", pod.Namespace, pod.Name),
					)
				}
				continue
			}

			// Failure state change
			if reason != prev.failReason {
				if reason == "" {
					// recovered — no notification needed
				} else {
					switch reason {
					case "CrashLoopBackOff":
						if p.PodCrashLoops {
							d.notify(
								fmt.Sprintf("pod-crash-%s-%s", pod.Namespace, pod.Name),
								fmt.Sprintf("Crash loop — %s", d.clusterName()),
								fmt.Sprintf("%s/%s is in CrashLoopBackOff", pod.Namespace, pod.Name),
							)
						}
					case "OOMKilled":
						if p.PodFailures {
							d.notify(
								fmt.Sprintf("pod-oom-%s-%s", pod.Namespace, pod.Name),
								fmt.Sprintf("Pod OOMKilled — %s", d.clusterName()),
								fmt.Sprintf("%s/%s ran out of memory", pod.Namespace, pod.Name),
							)
						}
					default:
						if p.PodFailures {
							d.notify(
								fmt.Sprintf("pod-fail-%s-%s", pod.Namespace, pod.Name),
								fmt.Sprintf("Pod failing — %s", d.clusterName()),
								fmt.Sprintf("%s/%s: %s", pod.Namespace, pod.Name, reason),
							)
						}
					}
				}
			}

			// Container restart increase
			if p.ContainerRestarts && restarts > prev.restartCount {
				d.notify(
					fmt.Sprintf("pod-restart-%s-%s", pod.Namespace, pod.Name),
					fmt.Sprintf("Container restarted — %s", d.clusterName()),
					fmt.Sprintf("%s/%s restarted (total restarts: %d)", pod.Namespace, pod.Name, restarts),
				)
			}
		}
	})
}

func podFailureReason(pod *corev1.Pod) string {
	if pod.DeletionTimestamp != nil {
		return "" // terminating pods are intentional
	}
	if pod.Status.Phase == corev1.PodFailed {
		if pod.Status.Reason != "" {
			return pod.Status.Reason
		}
		return "Failed"
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			switch cs.State.Waiting.Reason {
			case "CrashLoopBackOff", "Error", "OOMKilled", "ImagePullBackOff", "ErrImagePull":
				return cs.State.Waiting.Reason
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			if cs.State.Terminated.Reason == "OOMKilled" {
				return "OOMKilled"
			}
		}
	}
	return ""
}

func podTotalRestarts(pod *corev1.Pod) int32 {
	var total int32
	for _, cs := range pod.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	return total
}

// ─── Deployment watcher ─────────────────────────────────────────────────────

func (d *Dispatcher) watchDeployments(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*appsv1.Deployment{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, prop); err != nil {
		return
	}
	lastUnavail := map[types.UID]int32{}
	var seeded bool
	prop.Sub(ctx, func(deps []*appsv1.Deployment) {
		defer func() { seeded = true }()
		for _, dep := range deps {
			unavail := dep.Status.UnavailableReplicas
			if !seeded {
				lastUnavail[dep.UID] = unavail
				continue
			}
			p := d.prefs()
			if !p.Enabled || d.excluded(dep.Namespace, dep.Name) {
				lastUnavail[dep.UID] = unavail
				continue
			}
			prev, seen := lastUnavail[dep.UID]
			lastUnavail[dep.UID] = unavail
			if !seen {
				if p.NewDeployment {
					d.notify(
						fmt.Sprintf("deploy-new-%s-%s", dep.Namespace, dep.Name),
						fmt.Sprintf("New deployment — %s", d.clusterName()),
						fmt.Sprintf("Deployment %s/%s created", dep.Namespace, dep.Name),
					)
				}
				continue
			}
			if !p.DeploymentRollouts || unavail == 0 || prev == unavail {
				continue
			}
			if prev == 0 {
				d.notify(
					fmt.Sprintf("deploy-%s-%s", dep.Namespace, dep.Name),
					fmt.Sprintf("Deployment unavailable — %s", d.clusterName()),
					fmt.Sprintf("%s/%s has %d unavailable replica(s)", dep.Namespace, dep.Name, unavail),
				)
			}
		}
	})
}

// ─── StatefulSet watcher ────────────────────────────────────────────────────

func (d *Dispatcher) watchStatefulSets(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*appsv1.StatefulSet{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, prop); err != nil {
		return
	}
	known := map[types.UID]struct{}{}
	var seeded bool
	prop.Sub(ctx, func(sets []*appsv1.StatefulSet) {
		defer func() { seeded = true }()
		for _, ss := range sets {
			if !seeded {
				known[ss.UID] = struct{}{}
				continue
			}
			p := d.prefs()
			if !p.Enabled || !p.NewDeployment || d.excluded(ss.Namespace, ss.Name) {
				known[ss.UID] = struct{}{}
				continue
			}
			if _, seen := known[ss.UID]; !seen {
				known[ss.UID] = struct{}{}
				d.notify(
					fmt.Sprintf("sts-new-%s-%s", ss.Namespace, ss.Name),
					fmt.Sprintf("New StatefulSet — %s", d.clusterName()),
					fmt.Sprintf("StatefulSet %s/%s created", ss.Namespace, ss.Name),
				)
			}
		}
	})
}

// ─── DaemonSet watcher ──────────────────────────────────────────────────────

func (d *Dispatcher) watchDaemonSets(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*appsv1.DaemonSet{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, prop); err != nil {
		return
	}
	known := map[types.UID]struct{}{}
	var seeded bool
	prop.Sub(ctx, func(sets []*appsv1.DaemonSet) {
		defer func() { seeded = true }()
		for _, ds := range sets {
			if !seeded {
				known[ds.UID] = struct{}{}
				continue
			}
			p := d.prefs()
			if !p.Enabled || !p.NewDeployment || d.excluded(ds.Namespace, ds.Name) {
				known[ds.UID] = struct{}{}
				continue
			}
			if _, seen := known[ds.UID]; !seen {
				known[ds.UID] = struct{}{}
				d.notify(
					fmt.Sprintf("ds-new-%s-%s", ds.Namespace, ds.Name),
					fmt.Sprintf("New DaemonSet — %s", d.clusterName()),
					fmt.Sprintf("DaemonSet %s/%s created", ds.Namespace, ds.Name),
				)
			}
		}
	})
}

// ─── Node watcher ───────────────────────────────────────────────────────────

func (d *Dispatcher) watchNodes(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*corev1.Node{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, prop); err != nil {
		return
	}
	lastReady := map[types.UID]bool{}
	var seeded bool
	prop.Sub(ctx, func(nodes []*corev1.Node) {
		defer func() { seeded = true }()
		for _, node := range nodes {
			ready := nodeIsReady(node)
			if !seeded {
				lastReady[node.UID] = ready
				continue
			}
			p := d.prefs()
			if !p.Enabled {
				lastReady[node.UID] = ready
				continue
			}
			prev, seen := lastReady[node.UID]
			lastReady[node.UID] = ready
			if !seen {
				if p.NewNode {
					d.notify(
						fmt.Sprintf("node-new-%s", node.Name),
						fmt.Sprintf("Node joined — %s", d.clusterName()),
						fmt.Sprintf("Node %s joined the cluster", node.Name),
					)
				}
				continue
			}
			// ready → not-ready transition
			if p.NodeNotReady && !ready && prev {
				d.notify(
					fmt.Sprintf("node-%s", node.Name),
					fmt.Sprintf("Node unavailable — %s", d.clusterName()),
					fmt.Sprintf("Node %s is NotReady", node.Name),
				)
			}
		}
	})
}

func nodeIsReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ─── Job watcher ────────────────────────────────────────────────────────────

type jobState struct{ failed, complete bool }

func (d *Dispatcher) watchJobs(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*batchv1.Job{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, prop); err != nil {
		return
	}
	known := map[types.UID]jobState{}
	var seeded bool
	prop.Sub(ctx, func(jobs []*batchv1.Job) {
		defer func() { seeded = true }()
		for _, job := range jobs {
			cur := jobState{failed: jobFailed(job), complete: jobComplete(job)}
			if !seeded {
				known[job.UID] = cur
				continue
			}
			p := d.prefs()
			if !p.Enabled || d.excluded(job.Namespace, job.Name) {
				known[job.UID] = cur
				continue
			}
			prev, seen := known[job.UID]
			known[job.UID] = cur
			if !seen {
				continue
			}
			if p.JobFailures && cur.failed && !prev.failed {
				d.notify(
					fmt.Sprintf("job-%s-%s", job.Namespace, job.Name),
					fmt.Sprintf("Job failed — %s", d.clusterName()),
					fmt.Sprintf("Job %s/%s failed", job.Namespace, job.Name),
				)
			}
			if p.JobCompleted && cur.complete && !prev.complete {
				d.notify(
					fmt.Sprintf("job-done-%s-%s", job.Namespace, job.Name),
					fmt.Sprintf("Job completed — %s", d.clusterName()),
					fmt.Sprintf("Job %s/%s completed successfully", job.Namespace, job.Name),
				)
			}
		}
	})
}

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// ─── PVC watcher ────────────────────────────────────────────────────────────

func (d *Dispatcher) watchPVCs(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*corev1.PersistentVolumeClaim{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}, prop); err != nil {
		return
	}
	lastPhase := map[types.UID]corev1.PersistentVolumeClaimPhase{}
	var seeded bool
	prop.Sub(ctx, func(pvcs []*corev1.PersistentVolumeClaim) {
		defer func() { seeded = true }()
		for _, pvc := range pvcs {
			phase := pvc.Status.Phase
			if !seeded {
				lastPhase[pvc.UID] = phase
				continue
			}
			p := d.prefs()
			if !p.Enabled || !p.PVCPending || d.excluded(pvc.Namespace, pvc.Name) {
				lastPhase[pvc.UID] = phase
				continue
			}
			prev, seen := lastPhase[pvc.UID]
			lastPhase[pvc.UID] = phase
			if !seen || prev == phase {
				continue
			}
			if phase == corev1.ClaimPending {
				d.notify(
					fmt.Sprintf("pvc-%s-%s", pvc.Namespace, pvc.Name),
					fmt.Sprintf("PVC pending — %s", d.clusterName()),
					fmt.Sprintf("%s/%s cannot be bound", pvc.Namespace, pvc.Name),
				)
			}
		}
	})
}

// ─── Argo CD watcher ────────────────────────────────────────────────────────

type argoState struct{ sync, health string }

func (d *Dispatcher) watchArgo(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]client.Object{})
	if err := api.InformerConnectProperty(ctx, cluster, gitopsbe.GVRApplication, prop); err != nil {
		return
	}
	lastState := map[types.UID]argoState{}
	var seeded bool
	prop.Sub(ctx, func(objs []client.Object) {
		defer func() { seeded = true }()
		for _, obj := range objs {
			st := gitopsbe.ReadArgoStatus(obj)
			cur := argoState{sync: st.Sync, health: st.Health}
			if !seeded {
				lastState[obj.GetUID()] = cur
				continue
			}
			p := d.prefs()
			if !p.Enabled {
				lastState[obj.GetUID()] = cur
				continue
			}
			prev, seen := lastState[obj.GetUID()]
			lastState[obj.GetUID()] = cur
			if !seen {
				continue
			}
			name := obj.GetName()
			if p.ArgoAppDegraded && cur.health == "Degraded" && prev.health != "Degraded" {
				d.notify(
					fmt.Sprintf("argo-degraded-%s", name),
					fmt.Sprintf("App degraded — %s", d.clusterName()),
					fmt.Sprintf("Argo CD application %q health is Degraded", name),
				)
			}
			if p.ArgoAppOutOfSync && cur.sync == "OutOfSync" && prev.sync != "OutOfSync" {
				d.notify(
					fmt.Sprintf("argo-outofsync-%s", name),
					fmt.Sprintf("App out of sync — %s", d.clusterName()),
					fmt.Sprintf("Argo CD application %q is OutOfSync (rev %s)", name, st.Revision),
				)
			}
		}
	})
}

// ─── Flux CD watchers ───────────────────────────────────────────────────────

func (d *Dispatcher) watchFluxGVR(
	ctx context.Context,
	cluster *api.Cluster,
	gvr schema.GroupVersionResource,
	kind string,
	enabled func(api.NotificationPreferences) bool,
) {
	prop := pubsub.NewProperty([]client.Object{})
	if err := api.InformerConnectProperty(ctx, cluster, gvr, prop); err != nil {
		return
	}
	lastHealth := map[types.UID]gitopsbe.Health{}
	var seeded bool
	prop.Sub(ctx, func(objs []client.Object) {
		defer func() { seeded = true }()
		for _, obj := range objs {
			st := gitopsbe.ReadFluxStatus(obj)
			cur := st.HealthClass()
			if !seeded {
				lastHealth[obj.GetUID()] = cur
				continue
			}
			p := d.prefs()
			if !p.Enabled || !enabled(p) || d.excluded(obj.GetNamespace(), obj.GetName()) {
				lastHealth[obj.GetUID()] = cur
				continue
			}
			prev, seen := lastHealth[obj.GetUID()]
			lastHealth[obj.GetUID()] = cur
			if !seen || cur != gitopsbe.HealthDegraded || prev == gitopsbe.HealthDegraded {
				continue
			}
			msg := st.Message
			if msg == "" {
				msg = "reconciliation failed"
			}
			d.notify(
				fmt.Sprintf("flux-%s-%s-%s", gvr.Resource, obj.GetNamespace(), obj.GetName()),
				fmt.Sprintf("%s failed — %s", kind, d.clusterName()),
				fmt.Sprintf("%s/%s: %s", obj.GetNamespace(), obj.GetName(), msg),
			)
		}
	})
}

func (d *Dispatcher) watchFluxSource(
	ctx context.Context,
	cluster *api.Cluster,
	gvr schema.GroupVersionResource,
	kind string,
) {
	d.watchFluxGVR(ctx, cluster, gvr, kind,
		func(p api.NotificationPreferences) bool { return p.FluxSourceNotReady })
}

// ─── ConfigMap watcher ──────────────────────────────────────────────────────

func (d *Dispatcher) watchConfigMaps(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*corev1.ConfigMap{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, prop); err != nil {
		return
	}
	// track resource version to detect updates
	known := map[types.UID]string{} // uid → resourceVersion
	var seeded bool
	prop.Sub(ctx, func(cms []*corev1.ConfigMap) {
		defer func() { seeded = true }()
		for _, cm := range cms {
			if !seeded {
				known[cm.UID] = cm.ResourceVersion
				continue
			}
			p := d.prefs()
			if !p.Enabled || !p.ConfigChange || d.excluded(cm.Namespace, cm.Name) {
				known[cm.UID] = cm.ResourceVersion
				continue
			}
			prev, seen := known[cm.UID]
			known[cm.UID] = cm.ResourceVersion
			if !seen || prev == cm.ResourceVersion {
				continue
			}
			d.notify(
				fmt.Sprintf("cm-changed-%s-%s", cm.Namespace, cm.Name),
				fmt.Sprintf("ConfigMap updated — %s", d.clusterName()),
				fmt.Sprintf("%s/%s was modified", cm.Namespace, cm.Name),
			)
		}
	})
}

// ─── Secret watcher ─────────────────────────────────────────────────────────

func (d *Dispatcher) watchSecrets(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*corev1.Secret{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, prop); err != nil {
		return
	}
	known := map[types.UID]string{}
	var seeded bool
	prop.Sub(ctx, func(secrets []*corev1.Secret) {
		defer func() { seeded = true }()
		for _, sec := range secrets {
			if !seeded {
				known[sec.UID] = sec.ResourceVersion
				continue
			}
			p := d.prefs()
			if !p.Enabled || !p.ConfigChange || d.excluded(sec.Namespace, sec.Name) {
				known[sec.UID] = sec.ResourceVersion
				continue
			}
			prev, seen := known[sec.UID]
			known[sec.UID] = sec.ResourceVersion
			if !seen || prev == sec.ResourceVersion {
				continue
			}
			d.notify(
				fmt.Sprintf("secret-changed-%s-%s", sec.Namespace, sec.Name),
				fmt.Sprintf("Secret updated — %s", d.clusterName()),
				fmt.Sprintf("%s/%s was modified", sec.Namespace, sec.Name),
			)
		}
	})
}

// ─── Namespace watcher ──────────────────────────────────────────────────────

func (d *Dispatcher) watchNamespaces(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*corev1.Namespace{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, prop); err != nil {
		return
	}
	known := map[types.UID]struct{}{}
	var seeded bool
	prop.Sub(ctx, func(nss []*corev1.Namespace) {
		defer func() { seeded = true }()
		for _, ns := range nss {
			if !seeded {
				known[ns.UID] = struct{}{}
				continue
			}
			p := d.prefs()
			if !p.Enabled || !p.NewNamespace {
				known[ns.UID] = struct{}{}
				continue
			}
			if _, seen := known[ns.UID]; !seen {
				known[ns.UID] = struct{}{}
				d.notify(
					fmt.Sprintf("ns-new-%s", ns.Name),
					fmt.Sprintf("Namespace created — %s", d.clusterName()),
					fmt.Sprintf("Namespace %q was created", ns.Name),
				)
			}
		}
	})
}

// ─── Ingress watcher ────────────────────────────────────────────────────────

func (d *Dispatcher) watchIngresses(ctx context.Context, cluster *api.Cluster) {
	prop := pubsub.NewProperty([]*networkingv1.Ingress{})
	if err := api.InformerConnectProperty(ctx, cluster,
		schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, prop); err != nil {
		return
	}
	known := map[types.UID]struct{}{}
	var seeded bool
	prop.Sub(ctx, func(ings []*networkingv1.Ingress) {
		defer func() { seeded = true }()
		for _, ing := range ings {
			if !seeded {
				known[ing.UID] = struct{}{}
				continue
			}
			p := d.prefs()
			if !p.Enabled || !p.NewIngress || d.excluded(ing.Namespace, ing.Name) {
				known[ing.UID] = struct{}{}
				continue
			}
			if _, seen := known[ing.UID]; !seen {
				known[ing.UID] = struct{}{}
				d.notify(
					fmt.Sprintf("ingress-new-%s-%s", ing.Namespace, ing.Name),
					fmt.Sprintf("Ingress created — %s", d.clusterName()),
					fmt.Sprintf("Ingress %s/%s was created", ing.Namespace, ing.Name),
				)
			}
		}
	})
}
