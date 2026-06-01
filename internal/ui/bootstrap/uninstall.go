package bootstrap

import (
	"context"
	"fmt"

	"github.com/SilkePilon/Orchestrator/api"
	core "github.com/SilkePilon/Orchestrator/internal/bootstrap"
	"github.com/SilkePilon/Orchestrator/internal/pubsub"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func NewUninstallWizard(ctx context.Context, state *common.State, cluster api.ClusterPreferences, onFinish func()) *Wizard {
	w := &Wizard{
		ctx:                      ctx,
		state:                    state,
		draft:                    pubsub.NewProperty(uninstallDraft(cluster)),
		requireKubeconfig:        false,
		finishSuccessTitle:       "Cluster uninstalled",
		finishSuccessDescription: "k3s and related Kubernetes files were removed from the selected nodes.",
		onApplySuccess:           onFinish,
	}

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	w.NavigationPage = adw.NewNavigationPage(box, "Uninstall Cluster")

	w.toast = adw.NewToastOverlay()
	box.Append(w.toast)

	w.nav = adw.NewNavigationView()
	w.toast.SetChild(w.nav)
	w.nav.Add(w.uninstallNodesPage())

	return w
}

func (w *Wizard) uninstallNodesPage() *adw.NavigationPage {
	return w.nodesPageWithContinue("Review Uninstall", func() {
		d := w.draft.Value()
		for _, n := range d.Nodes {
			if n.Host == "" {
				w.errorToast(fmt.Errorf("node %q has no host", labelOr(n)))
				return
			}
			if n.User == "" {
				w.errorToast(fmt.Errorf("node %q has no user", labelOr(n)))
				return
			}
		}
		w.push(w.uninstallPlanPage())
	})
}

func uninstallDraft(cluster api.ClusterPreferences) *core.BootstrapDraft {
	d := &core.BootstrapDraft{
		Options: core.K3sOptions{ClusterName: cluster.Name},
		Probes:  map[string]*core.NodeProbe{},
	}
	if cluster.Bootstrap == nil {
		return d
	}

	if len(cluster.Bootstrap.Nodes) > 0 {
		for _, rec := range cluster.Bootstrap.Nodes {
			node := core.NewNode(roleFromRecord(rec.Role))
			node.Host = rec.Host
			node.Port = rec.Port
			if node.Port == 0 {
				node.Port = 22
			}
			node.User = rec.User
			if node.User == "" {
				node.User = "root"
			}
			node.Auth = authFromRecord(rec.Auth)
			node.PrivateKeyPath = rec.PrivateKeyPath
			node.Become = becomeFromRecord(rec.Become)
			node.Label = rec.Label
			d.Nodes = append(d.Nodes, node)
		}
		return d
	}

	server := core.NewNode(core.RoleServer)
	server.Host = cluster.Bootstrap.ServerHost
	server.Label = "server-1"
	d.Nodes = append(d.Nodes, server)
	for i, host := range cluster.Bootstrap.AgentHosts {
		agent := core.NewNode(core.RoleAgent)
		agent.Host = host
		agent.Label = fmt.Sprintf("agent-%d", i+1)
		d.Nodes = append(d.Nodes, agent)
	}
	return d
}

func roleFromRecord(role string) core.NodeRole {
	if role == string(core.RoleAgent) {
		return core.RoleAgent
	}
	return core.RoleServer
}

func authFromRecord(auth string) core.AuthMethod {
	switch auth {
	case string(core.AuthPassword):
		return core.AuthPassword
	case string(core.AuthPrivateKey):
		return core.AuthPrivateKey
	default:
		return core.AuthAgent
	}
}

func becomeFromRecord(become string) core.BecomeMethod {
	switch become {
	case string(core.BecomeSudo):
		return core.BecomeSudo
	case string(core.BecomeSu):
		return core.BecomeSu
	default:
		return core.BecomeNone
	}
}
