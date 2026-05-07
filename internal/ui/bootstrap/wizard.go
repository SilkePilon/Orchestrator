// Package bootstrap implements the Adwaita-based wizard UI for the k3s
// cluster bootstrapper. The pure-Go backend lives in
// internal/bootstrap.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	core "github.com/getseabird/seabird/internal/bootstrap"
	"github.com/getseabird/seabird/internal/ctxt"
	"github.com/getseabird/seabird/internal/pubsub"
	"github.com/getseabird/seabird/internal/ui/common"
)

// Wizard is the navigation root of the cluster-bootstrap experience.
// It owns the shared draft and the inner adw.NavigationView. The
// caller pushes the returned NavigationPage onto its own outer
// NavigationView.
type Wizard struct {
	*adw.NavigationPage
	ctx   context.Context
	state *common.State
	draft pubsub.Property[*core.BootstrapDraft]
	nav   *adw.NavigationView
	toast *adw.ToastOverlay

	// onFinish is called with the freshly-bootstrapped kubeconfig after
	// the user confirms on the Finish page. The welcome window passes a
	// callback that adds the cluster to preferences and opens it.
	onFinish FinishHandler
}

// FinishHandler is invoked from the Finish page once the bootstrap
// completes successfully and the user clicks "Open Cluster". It is
// expected to register a new ClusterPreferences and open the cluster
// window; on error it should surface a dialog.
type FinishHandler func(ctx context.Context, draft *core.BootstrapDraft, kubeconfigYAML string)

// NewWizard returns a fresh wizard rooted at the Intro page. Calling
// code is responsible for pushing wizard.NavigationPage onto its own
// adw.NavigationView (typically the welcome window's).
func NewWizard(ctx context.Context, state *common.State, onFinish FinishHandler) *Wizard {
	w := &Wizard{
		ctx:      ctx,
		state:    state,
		draft:    pubsub.NewProperty(newDraft()),
		onFinish: onFinish,
	}

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	w.NavigationPage = adw.NewNavigationPage(box, "Create Cluster")

	w.toast = adw.NewToastOverlay()
	box.Append(w.toast)

	w.nav = adw.NewNavigationView()
	w.toast.SetChild(w.nav)
	w.nav.Add(w.intro())

	return w
}

// newDraft creates a fresh BootstrapDraft pre-populated with one server
// node so the Nodes page is never empty.
func newDraft() *core.BootstrapDraft {
	srv := core.NewNode(core.RoleServer)
	srv.Label = "server-1"
	return &core.BootstrapDraft{
		Options: core.K3sOptions{
			ClusterName: "k3s",
			Channel:     "stable",
			CNI:         "flannel",
		},
		Nodes:  []core.Node{srv},
		Probes: map[string]*core.NodeProbe{},
	}
}

// pushPage helper: pushes a new NavigationPage onto the inner stack.
func (w *Wizard) push(p *adw.NavigationPage) { w.nav.Push(p) }

// pageShell builds the standard page chrome: header bar with a
// "Continue" button on the right that calls onContinue. The body
// (a PreferencesPage typically) is wrapped in a clamped vertical box.
func (w *Wizard) pageShell(title, continueLabel string, body gtk.Widgetter, onContinue func()) *adw.NavigationPage {
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	page := adw.NewNavigationPage(box, title)

	header := adw.NewHeaderBar()
	box.Append(header)

	if onContinue != nil {
		btn := gtk.NewButtonWithLabel(continueLabel)
		btn.AddCSSClass("suggested-action")
		btn.ConnectClicked(onContinue)
		header.PackEnd(btn)
	}

	box.Append(body)
	return page
}

// errorToast surfaces a non-fatal error in the wizard's toast overlay.
func (w *Wizard) errorToast(err error) {
	if err == nil {
		return
	}
	w.toast.AddToast(adw.NewToast(fmt.Sprintf("Error: %s", err)))
}

// parentWindow is a convenience for dialog parents; reads from ctx.
func (w *Wizard) parentWindow() *gtk.Window {
	return ctxt.MustFrom[*gtk.Window](w.ctx)
}
