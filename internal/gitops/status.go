package gitops

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Health is a coarse status classification used to colour the UI.
type Health int

const (
	HealthUnknown Health = iota
	HealthOK
	HealthProgressing
	HealthDegraded
	HealthSuspended
)

// CSSClass maps a Health to an Adwaita status CSS class.
func (h Health) CSSClass() string {
	switch h {
	case HealthOK:
		return "success"
	case HealthProgressing:
		return "accent"
	case HealthDegraded:
		return "error"
	case HealthSuspended:
		return "warning"
	default:
		return "dim-label"
	}
}

func asUnstructured(o client.Object) *unstructured.Unstructured {
	if u, ok := o.(*unstructured.Unstructured); ok {
		return u
	}
	return nil
}

// ArgoStatus describes the sync/health of an Argo CD Application.
type ArgoStatus struct {
	Sync     string
	Health   string
	Revision string
}

// ReadArgoStatus extracts sync/health from an Application object.
func ReadArgoStatus(o client.Object) ArgoStatus {
	u := asUnstructured(o)
	if u == nil {
		return ArgoStatus{}
	}
	sync, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	rev, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision")
	if len(rev) > 7 {
		rev = rev[:7]
	}
	return ArgoStatus{Sync: sync, Health: health, Revision: rev}
}

// Health classifies an Argo Application.
func (s ArgoStatus) HealthClass() Health {
	switch s.Health {
	case "Healthy":
		if s.Sync == "Synced" {
			return HealthOK
		}
		return HealthProgressing
	case "Progressing", "Missing":
		return HealthProgressing
	case "Degraded":
		return HealthDegraded
	case "Suspended":
		return HealthSuspended
	default:
		return HealthUnknown
	}
}

// ArgoSource extracts repo/path/revision from an Application spec.
func ArgoSource(o client.Object) (repoURL, path, targetRevision, destNamespace string) {
	u := asUnstructured(o)
	if u == nil {
		return
	}
	repoURL, _, _ = unstructured.NestedString(u.Object, "spec", "source", "repoURL")
	path, _, _ = unstructured.NestedString(u.Object, "spec", "source", "path")
	targetRevision, _, _ = unstructured.NestedString(u.Object, "spec", "source", "targetRevision")
	destNamespace, _, _ = unstructured.NestedString(u.Object, "spec", "destination", "namespace")
	return
}

// FluxStatus describes the readiness of a Flux object.
type FluxStatus struct {
	Ready     string // "True"/"False"/"Unknown"
	Message   string
	Suspended bool
	Revision  string
}

// ReadFluxStatus extracts the Ready condition and suspend flag.
func ReadFluxStatus(o client.Object) FluxStatus {
	u := asUnstructured(o)
	if u == nil {
		return FluxStatus{}
	}
	var st FluxStatus
	st.Suspended, _, _ = unstructured.NestedBool(u.Object, "spec", "suspend")
	st.Revision, _, _ = unstructured.NestedString(u.Object, "status", "lastAppliedRevision")
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if found {
		for _, ci := range conds {
			cm, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			t, _, _ := unstructured.NestedString(cm, "type")
			if t != "Ready" {
				continue
			}
			st.Ready, _, _ = unstructured.NestedString(cm, "status")
			st.Message, _, _ = unstructured.NestedString(cm, "message")
		}
	}
	return st
}

// HealthClass classifies a Flux object.
func (s FluxStatus) HealthClass() Health {
	if s.Suspended {
		return HealthSuspended
	}
	switch s.Ready {
	case "True":
		return HealthOK
	case "False":
		return HealthDegraded
	case "Unknown", "":
		return HealthProgressing
	default:
		return HealthUnknown
	}
}

// ReadyLabel returns a short, human-readable status string for a Flux object.
func (s FluxStatus) ReadyLabel() string {
	if s.Suspended {
		return "Suspended"
	}
	switch s.Ready {
	case "True":
		return "Ready"
	case "False":
		return "Failed"
	default:
		return "Reconciling"
	}
}

// FluxSource extracts the source URL/ref for a GitRepository.
func FluxGitSource(o client.Object) (url, branch string) {
	u := asUnstructured(o)
	if u == nil {
		return
	}
	url, _, _ = unstructured.NestedString(u.Object, "spec", "url")
	branch, _, _ = unstructured.NestedString(u.Object, "spec", "ref", "branch")
	return
}
