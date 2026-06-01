package single

import (
	"context"
	"fmt"
	"sort"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/pubsub"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/internal/ui/editor"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/google/uuid"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type SingleView struct {
	*adw.NavigationPage
	*common.ClusterState
	ctx          context.Context
	prefPage     *adw.PreferencesPage
	groups       []*adw.PreferencesGroup
	loadingGroup *adw.PreferencesGroup
	sourceBuffer *gtksource.Buffer
	sourceView   *gtksource.View
	stack        *adw.ViewStack
	editor       *editor.EditorWindow
	navView      *adw.NavigationView

	// pendingObject holds the object whose YAML has not yet been loaded into the
	// source buffer. Serialising and syntax-highlighting large objects (e.g. Argo
	// CD Applications, which also update frequently) is expensive, so it is
	// deferred until the Yaml tab is actually visible.
	pendingObject  client.Object
	sourceRendered bool

	// propGen is incremented on every selection change so that asynchronously
	// built properties from a previous selection can be discarded.
	propGen int

	PinAdded   pubsub.Topic[client.Object]
	PinRemoved pubsub.Topic[client.Object]
	Deleted    pubsub.Topic[client.Object]
}

func NewSingleView(ctx context.Context, state *common.ClusterState, editor *editor.EditorWindow, navView *adw.NavigationView) *SingleView {
	content := gtk.NewBox(gtk.OrientationVertical, 0)
	content.AddCSSClass("view")
	view := SingleView{
		NavigationPage: adw.NewNavigationPage(content, "Object"),
		ClusterState:   state,
		prefPage:       adw.NewPreferencesPage(),
		ctx:            ctx,
		editor:         editor,
		navView:        navView,
		PinAdded:       pubsub.NewTopic[client.Object](),
		PinRemoved:     pubsub.NewTopic[client.Object](),
		Deleted:        pubsub.NewTopic[client.Object](),
	}
	view.SetTag(uuid.NewString())

	if sw, ok := view.prefPage.FirstChild().(*gtk.ScrolledWindow); ok {
		if vp, ok := sw.FirstChild().(*gtk.Viewport); ok {
			if clamp, ok := vp.FirstChild().(*adw.Clamp); ok {
				clamp.SetMaximumSize(5000)
			}
		}
	}

	header := adw.NewHeaderBar()
	header.AddCSSClass("flat")
	content.Append(header)

	delete := gtk.NewButton()
	delete.SetIconName("user-trash-symbolic")
	delete.SetTooltipText("Delete")
	delete.ConnectClicked(func() {
		selected := view.SelectedObject.Value()
		dialog := adw.NewAlertDialog(fmt.Sprintf("Delete %s?", selected.GetObjectKind().GroupVersionKind().Kind), selected.GetName())
		defer dialog.Present(view)
		dialog.AddResponse("cancel", "Cancel")
		dialog.AddResponse("delete", "Delete")
		dialog.SetResponseAppearance("delete", adw.ResponseDestructive)
		dialog.ConnectResponse(func(response string) {
			switch response {
			case "delete":
				if err := view.Delete(ctx, selected); err != nil {
					widget.ShowErrorDialog(ctx, "Failed to delete object", err)
				}
			}
		})
	})
	header.PackEnd(delete)

	edit := gtk.NewButton()
	edit.SetIconName("text-editor-symbolic")
	edit.SetTooltipText("Edit")
	edit.ConnectClicked(func() {
		gvk := view.SelectedObject.Value().GetObjectKind().GroupVersionKind()
		if err := view.editor.AddPage(&gvk, view.SelectedObject.Value()); err != nil {
			widget.ShowErrorDialog(view.ctx, "Error loading editor", err)
		} else {
			view.editor.Present()
		}
	})
	header.PackEnd(edit)

	pin := gtk.NewToggleButton()
	pin.SetIconName("star-outline-rounded-symbolic")
	pin.SetTooltipText("Pin")
	pin.ConnectClicked(func() {
		if pin.Active() {
			view.PinAdded.Pub(view.SelectedObject.Value())
		} else {
			view.PinRemoved.Pub(view.SelectedObject.Value())
		}
	})
	header.PackEnd(pin)

	kind := gtk.NewLabel("")
	kind.SetEllipsize(pango.EllipsizeEnd)
	kind.SetHAlign(gtk.AlignStart)
	kind.AddCSSClass("title-4")
	kind.SetMarginStart(10)
	kind.SetVExpand(true)
	header.PackStart(kind)

	stack := adw.NewViewStack()
	stack.AddTitledWithIcon(view.prefPage, "properties", "Properties", "view-list-bullet-symbolic")
	stack.AddTitledWithIcon(view.createSource(), "source", "Yaml", "code-symbolic")
	content.Append(stack)
	view.stack = stack
	stack.NotifyProperty("visible-child-name", func() {
		view.maybeRenderSource()
	})

	switcher := adw.NewViewSwitcher()
	switcher.SetPolicy(adw.ViewSwitcherPolicyWide)
	switcher.SetStack(stack)
	header.SetTitleWidget(switcher)

	view.ClusterPreferences.Sub(ctx, func(prefs api.ClusterPreferences) {
		edit.SetVisible(!prefs.ReadOnly)
		delete.SetVisible(!prefs.ReadOnly)

		if object := view.SelectedObject.Value(); object != nil {
			pinned := false
			for _, p := range prefs.Navigation.Pins {
				if p.Name == object.GetName() && p.Namespace == object.GetNamespace() {
					pinned = true
					break
				}
			}
			pin.SetActive(pinned)
		}
	})

	watchCtx, cancelWatch := context.WithCancel(ctx)
	view.SelectedObject.Sub(ctx, func(object client.Object) {
		if object == nil {
			view.pendingObject = nil
			view.sourceRendered = true
			view.sourceBuffer.SetText("")
			view.updateProperties([]api.Property{})
			if visible := view.navView.VisiblePage(); visible != nil && visible.Tag() == view.Tag() {
				view.navView.Pop()
			}
			return
		}

		kind.SetText(object.GetObjectKind().GroupVersionKind().Kind)

		cancelWatch()
		watchCtx, cancelWatch = context.WithCancel(ctx)
		gvr, _ := view.Cluster.GVKToR(object.GetObjectKind().GroupVersionKind())
		if gvr != nil {
			view.Cluster.AddInformerEventHandler(watchCtx, *gvr, cache.ResourceEventHandlerFuncs{
				UpdateFunc: func(_, new interface{}) {
					obj := new.(client.Object)
					if obj.GetUID() != object.GetUID() {
						return
					}
					view.SelectedObject.Pub(obj)
				},
				DeleteFunc: func(obj interface{}) {
					if obj.(client.Object).GetUID() != object.GetUID() {
						return
					}
					ctxt.MustFrom[*adw.ToastOverlay](ctx).AddToast(adw.NewToast(fmt.Sprintf("%v was deleted", object.GetName())))
					glib.IdleAdd(func() {
						if pin.Active() {
							view.PinRemoved.Pub(object)
						}
						view.Deleted.Pub(object)
					})
				},
			})
		}

		resource := view.GetAPIResource(object.GetObjectKind().GroupVersionKind())

		view.pendingObject = object
		view.sourceRendered = false
		view.maybeRenderSource()

		// Building properties can make blocking network calls (e.g. resolving
		// env-var ConfigMap/Secret references for Pods), so do it off the main
		// thread to keep the UI responsive. A generation counter discards results
		// from selections that have since been superseded.
		view.propGen++
		gen := view.propGen
		buildCtx := watchCtx
		view.updateProperties([]api.Property{})
		view.setLoading(true)
		go func() {
			var props []api.Property
			for _, ext := range view.Extensions {
				if buildCtx.Err() != nil {
					return
				}
				props = ext.CreateObjectProperties(buildCtx, resource, object, props)
			}
			sort.Slice(props, func(i, j int) bool {
				return props[i].GetPriority() > props[j].GetPriority()
			})
			glib.IdleAdd(func() {
				if view.propGen != gen {
					return
				}
				view.updateProperties(props)
			})
		}()

		pinned := false
		for _, p := range view.ClusterPreferences.Value().Navigation.Pins {
			if p.Name == object.GetName() && p.Namespace == object.GetNamespace() {
				pinned = true
				break
			}
		}
		pin.SetActive(pinned)
	})

	return &view
}

func (view *SingleView) maybeRenderSource() {
	if view.sourceRendered || view.sourceBuffer == nil {
		return
	}
	if view.stack == nil || view.stack.VisibleChildName() != "source" {
		return
	}
	view.sourceRendered = true

	object := view.pendingObject
	if object == nil {
		view.sourceBuffer.SetText("")
		return
	}
	yaml, err := view.Encoder.EncodeYAML(object)
	if err != nil {
		view.sourceBuffer.SetText(fmt.Sprintf("error: %v", err))
	} else {
		view.sourceBuffer.SetText(string(yaml))
	}
}

func (view *SingleView) updateProperties(properties []api.Property) {
	view.setLoading(false)
	for _, g := range view.groups {
		view.prefPage.Remove(g)
	}
	view.groups = nil

	for _, prop := range properties {
		pv := propertiesView{view.Cluster}
		group := pv.Render(view.ctx, 0, prop, view).(*adw.PreferencesGroup)
		view.groups = append(view.groups, group)
		view.prefPage.Add(group)
	}
}

// setLoading shows or hides a centered spinner with a "Loading…" label on the
// properties page, so the sheet isn't blank while properties are being built
// off the main thread.
func (view *SingleView) setLoading(loading bool) {
	if !loading {
		if view.loadingGroup != nil {
			view.prefPage.Remove(view.loadingGroup)
			view.loadingGroup = nil
		}
		return
	}
	if view.loadingGroup != nil {
		return
	}

	box := gtk.NewBox(gtk.OrientationVertical, 12)
	box.SetHAlign(gtk.AlignCenter)
	box.SetVAlign(gtk.AlignCenter)
	box.SetMarginTop(48)
	box.SetMarginBottom(48)

	spinner := gtk.NewSpinner()
	spinner.SetSizeRequest(32, 32)
	spinner.Start()
	box.Append(spinner)

	label := gtk.NewLabel("Loading…")
	label.AddCSSClass("dim-label")
	box.Append(label)

	group := adw.NewPreferencesGroup()
	group.Add(box)
	view.loadingGroup = group
	view.prefPage.Add(group)
}

func (view *SingleView) createSource() *gtk.ScrolledWindow {
	scrolledWindow := gtk.NewScrolledWindow()
	scrolledWindow.SetVExpand(true)

	view.sourceBuffer = gtksource.NewBufferWithLanguage(gtksource.LanguageManagerGetDefault().Language("yaml"))
	view.setSourceColorScheme()
	gtk.SettingsGetDefault().NotifyProperty("gtk-application-prefer-dark-theme", view.setSourceColorScheme)
	view.sourceView = gtksource.NewViewWithBuffer(view.sourceBuffer)
	view.sourceView.SetEditable(false)
	view.sourceView.SetWrapMode(gtk.WrapWord)
	view.sourceView.SetShowLineNumbers(true)
	view.sourceView.SetMonospace(true)
	scrolledWindow.SetChild(view.sourceView)

	return scrolledWindow
}

func (view *SingleView) setSourceColorScheme() {
	util.SetSourceColorScheme(view.sourceBuffer)
}
