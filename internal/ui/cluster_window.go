package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/internal/ui/editor"
	"github.com/SilkePilon/Orchestrator/internal/ui/gitops"
	"github.com/SilkePilon/Orchestrator/internal/ui/list"
	"github.com/SilkePilon/Orchestrator/internal/ui/single"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ClusterWindow struct {
	*adw.ApplicationWindow
	*common.ClusterState
	ctx          context.Context
	cancel       context.CancelFunc
	navigation   *Navigation
	listView     *list.List
	objectView   *single.SingleView
	toastOverlay *adw.ToastOverlay
	dialog       *adw.Dialog
}

func NewClusterWindow(ctx context.Context, app *gtk.Application, state *common.ClusterState) *ClusterWindow {
	window := adw.NewApplicationWindow(app)
	ctx = ctxt.With[*gtk.Window](ctx, &window.Window)
	ctx = ctxt.With[*api.Cluster](ctx, state.Cluster)
	ctx, cancel := context.WithCancel(ctx)
	w := ClusterWindow{
		ClusterState:      state,
		ctx:               ctx,
		ApplicationWindow: window,
		cancel:            cancel,
	}
	w.SetIconName("orchestrator")
	w.SetTitle(fmt.Sprintf("%s - %s", w.ClusterPreferences.Value().Name, ApplicationName))
	w.SetDefaultSize(1000, 600)

	var h glib.SignalHandle
	h = w.ConnectCloseRequest(func() bool {
		prefs := w.Preferences.Value()
		if err := prefs.Save(); err != nil {
			d := widget.ShowErrorDialog(ctx, "Could not save preferences", err)
			d.ConnectUnrealize(func() {
				w.HandlerDisconnect(h)
				w.Close()
			})
			return true
		}
		app := w.Application()
		if !isLastVisibleWindow(app) {
			return false
		}
		// Last visible window — ask the user what to do.
		dialog := adw.NewAlertDialog(
			"Keep Running in Background?",
			"Orchestrator can keep running in the background so you can reopen it quickly.",
		)
		dialog.AddResponse("quit", "Quit")
		dialog.AddResponse("background", "Run in Background")
		dialog.SetResponseAppearance("background", adw.ResponseSuggested)
		dialog.SetDefaultResponse("background")
		dialog.ConnectResponse(func(response string) {
			switch response {
			case "quit":
				w.HandlerDisconnect(h)
				w.Close()
			case "background":
				goBackground(app)
			}
		})
		dialog.Present(&w.Window)
		return true
	})

	w.toastOverlay = adw.NewToastOverlay()
	w.SetContent(w.toastOverlay)
	ctx = ctxt.With[*adw.ToastOverlay](ctx, w.toastOverlay)

	editor := editor.NewEditorWindow(ctx)

	viewStack := gtk.NewStack()
	viewStack.SetTransitionType(gtk.StackTransitionTypeCrossfade)

	paned := gtk.NewPaned(gtk.OrientationHorizontal)
	paned.SetPosition(225)
	paned.SetShrinkStartChild(false)
	paned.SetShrinkEndChild(false)
	w.toastOverlay.SetChild(paned)

	w.dialog = adw.NewDialog()
	w.dialog.SetPresentationMode(adw.DialogBottomSheet)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				glib.IdleAdd(func() {
					w.dialog.SetContentWidth(int(float64(w.Width()) * 0.7))
					w.dialog.SetContentHeight(int(float64(w.Height()) * 0.9))

				})
			}
			time.Sleep(time.Second)
		}
	}()

	w.navigation = NewNavigation(ctx, w.ClusterState, viewStack, editor)
	w.navigation.SetSizeRequest(225, -1)
	paned.SetStartChild(w.navigation)

	navView := adw.NewNavigationView()
	w.objectView = single.NewSingleView(ctx, w.ClusterState, editor, navView)
	w.objectView.PinAdded.Sub(ctx, func(obj client.Object) {
		if selected := w.objectView.SelectedObject.Value(); selected != nil && obj.GetUID() == selected.GetUID() {
			w.dialog.Close()
		}
		w.navigation.AddPin(obj)
	})
	w.objectView.PinRemoved.Sub(ctx, w.navigation.RemovePin)
	w.objectView.Deleted.Sub(ctx, func(object client.Object) {
		w.dialog.Close()
	})
	navView.Add(w.objectView.NavigationPage)
	w.dialog.SetChild(navView)
	w.dialog.ConnectClosed(func() {
		navView.ReplaceWithTags([]string{w.objectView.Tag()})
	})

	w.listView = list.NewList(ctx, w.ClusterState, w.dialog, editor)
	viewStack.AddChild(w.listView).SetName("list")
	viewStack.SetVisibleChild(w.listView)

	gitopsView := gitops.NewView(ctx, w.ClusterState, editor)
	viewStack.AddChild(gitopsView).SetName("gitops")

	paned.SetEndChild(viewStack)

	w.createActions()
	return &w
}

func (w *ClusterWindow) createActions() {
	newWindow := gio.NewSimpleAction("newWindow", nil)
	newWindow.ConnectActivate(func(_ *glib.Variant) {
		prefs, err := api.LoadPreferences()
		if err != nil {
			widget.ShowErrorDialog(w.ctx, "Could not load preferences", err)
			return
		}
		prefs.Defaults()
		NewWelcomeWindow(context.WithoutCancel(w.ctx), w.Application(), w.State).Present()
	})
	w.AddAction(newWindow)
	w.Application().SetAccelsForAction("win.newWindow", []string{"<Ctrl>N"})

	disconnect := gio.NewSimpleAction("disconnect", nil)
	disconnect.ConnectActivate(func(_ *glib.Variant) {
		w.ActivateAction("newWindow", nil)
		w.cancel()
		w.Close()
	})
	w.AddAction(disconnect)
	w.Application().SetAccelsForAction("win.disconnect", []string{"<Ctrl>Q"})

	action := gio.NewSimpleAction("prefs", nil)
	action.ConnectActivate(func(_ *glib.Variant) {
		prefs := NewPreferencesWindow(w.ctx, w.State)
		prefs.SetTransientFor(&w.Window)
		prefs.Present()
	})
	w.AddAction(action)

	action = gio.NewSimpleAction("about", nil)
	action.ConnectActivate(func(_ *glib.Variant) {
		NewAboutWindow().Present(&w.Window)
	})
	w.AddAction(action)

}
