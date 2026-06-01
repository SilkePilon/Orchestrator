// Package gitops renders the Adwaita "GitOps" view: an install experience for
// Argo CD / Flux CD and a management dashboard styled after the single-object
// (Node) view. The pure-Go backend lives in internal/gitops.
package gitops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	backend "github.com/SilkePilon/Orchestrator/internal/gitops"
	"github.com/SilkePilon/Orchestrator/internal/pubsub"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/internal/ui/editor"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/zmwangx/debounce"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// View is the root widget added to the cluster window's view stack.
type View struct {
	*adw.Bin
	ctx    context.Context
	state  *common.ClusterState
	editor *editor.EditorWindow

	// renderCancel cancels the informers/subscriptions of the current screen.
	renderCancel context.CancelFunc

	// install form state
	selProvider backend.Provider

	// argoWebURL caches the local URL of an active port-forward to the Argo CD
	// web UI so repeated clicks reuse the same tunnel.
	argoWebURL string
}

// NewView constructs the GitOps view and kicks off engine detection.
func NewView(ctx context.Context, state *common.ClusterState, editor *editor.EditorWindow) *View {
	v := &View{
		Bin:    adw.NewBin(),
		ctx:    ctx,
		state:  state,
		editor: editor,
	}
	v.AddCSSClass("view")
	v.refresh()
	return v
}

// refresh re-detects the installed engine and renders the appropriate screen.
func (v *View) refresh() {
	v.setRoot(v.loadingScreen("Checking for GitOps engines…"))
	go func() {
		det, err := backend.Detect(v.ctx, v.state.Cluster.Client)
		glib.IdleAdd(func() {
			if err != nil {
				v.setRoot(v.errorScreen("Could not query the cluster", err, v.refresh))
				return
			}
			switch {
			case det.Conflict():
				v.renderDashboard(det.Primary(), true)
			case det.Primary() != backend.ProviderNone:
				v.renderDashboard(det.Primary(), false)
			default:
				v.renderInstall()
			}
		})
	}()
}

// setRoot swaps the visible screen, cancelling any prior subscriptions.
func (v *View) setRoot(w gtk.Widgetter) {
	if v.renderCancel != nil {
		v.renderCancel()
		v.renderCancel = nil
	}
	v.SetChild(w)
}

func (v *View) newRenderCtx() context.Context {
	ctx, cancel := context.WithCancel(v.ctx)
	v.renderCancel = cancel
	return ctx
}

func (v *View) toast(msg string) {
	if to, ok := ctxt.From[*adw.ToastOverlay](v.ctx); ok {
		to.AddToast(adw.NewToast(msg))
	}
}

// --- generic chrome ------------------------------------------------------

func setSelectedClass(row *adw.ActionRow, selected bool) {
	if selected {
		row.AddCSSClass("accent")
	} else {
		row.RemoveCSSClass("accent")
	}
}

func shell(title string) (*gtk.Box, *adw.HeaderBar) {
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("view")
	header := adw.NewHeaderBar()
	header.AddCSSClass("flat")
	header.SetTitleWidget(adw.NewWindowTitle(title, ""))
	box.Append(header)
	return box, header
}

func (v *View) loadingScreen(msg string) gtk.Widgetter {
	box, _ := shell("GitOps")
	status := adw.NewStatusPage()
	status.SetVExpand(true)
	spinner := gtk.NewSpinner()
	spinner.SetSizeRequest(32, 32)
	spinner.Start()
	status.SetChild(spinner)
	status.SetDescription(msg)
	box.Append(status)
	return box
}

func (v *View) errorScreen(title string, err error, retry func()) gtk.Widgetter {
	box, _ := shell("GitOps")
	status := adw.NewStatusPage()
	status.SetVExpand(true)
	status.SetIconName("dialog-warning-symbolic")
	status.SetTitle(title)
	if err != nil {
		status.SetDescription(err.Error())
	}
	if retry != nil {
		btn := gtk.NewButtonWithLabel("Try Again")
		btn.AddCSSClass("pill")
		btn.AddCSSClass("suggested-action")
		btn.SetHAlign(gtk.AlignCenter)
		btn.ConnectClicked(func() { retry() })
		status.SetChild(btn)
	}
	box.Append(status)
	return box
}

// --- install screen ------------------------------------------------------

func (v *View) renderInstall() {
	v.setRoot(v.installScreen())
}

func (v *View) installScreen() gtk.Widgetter {
	box, header := shell("GitOps")

	installBtn := gtk.NewButtonWithLabel("Install")
	installBtn.AddCSSClass("suggested-action")
	installBtn.SetSensitive(false)
	header.PackEnd(installBtn)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	box.Append(scroll)

	// Intro.
	intro := adw.NewPreferencesGroup()
	intro.SetTitle("Set up GitOps")
	intro.SetDescription("Continuously deliver applications from Git. Choose one engine — only a single engine can be active per cluster.")
	page.Add(intro)

	// Engine choice.
	choice := adw.NewPreferencesGroup()
	choice.SetTitle("Engine")
	page.Add(choice)

	argoCheck := gtk.NewImageFromIconName("object-select-symbolic")
	argoCheck.SetVisible(false)
	fluxCheck := gtk.NewImageFromIconName("object-select-symbolic")
	fluxCheck.SetVisible(false)

	argoRow := adw.NewActionRow()
	argoRow.SetTitle("Argo CD")
	argoRow.SetSubtitle("Declarative GitOps with a rich UI and application sync")
	argoRow.SetActivatable(true)
	argoIcon := gtk.NewImageFromIconName("globe-symbolic")
	argoRow.AddPrefix(argoIcon)
	argoRow.AddSuffix(argoCheck)
	choice.Add(argoRow)

	fluxRow := adw.NewActionRow()
	fluxRow.SetTitle("Flux CD")
	fluxRow.SetSubtitle("Lightweight, controller-based GitOps toolkit")
	fluxRow.SetActivatable(true)
	fluxIcon := gtk.NewImageFromIconName("branch-symbolic")
	fluxRow.AddPrefix(fluxIcon)
	fluxRow.AddSuffix(fluxCheck)
	choice.Add(fluxRow)

	// Configuration.
	config := adw.NewPreferencesGroup()
	config.SetTitle("Configuration")
	page.Add(config)

	nsRow := adw.NewEntryRow()
	nsRow.SetTitle("Namespace")
	config.Add(nsRow)

	versionRow := adw.NewComboRow()
	versionRow.SetTitle("Version")
	versionRow.SetModel(gtk.NewStringList([]string{"—"}))
	versionRow.SetSensitive(false)
	config.Add(versionRow)

	haRow := adw.NewSwitchRow()
	haRow.SetTitle("High Availability")
	haRow.SetSubtitle("Deploy redundant Argo CD components (recommended for production)")
	config.Add(haRow)

	loadedVersionsFor := backend.ProviderNone
	loadVersions := func(provider backend.Provider) {
		if provider == loadedVersionsFor {
			return
		}
		loadedVersionsFor = provider
		def := "stable"
		if provider == backend.ProviderFlux {
			def = "latest"
		}
		versionRow.SetModel(gtk.NewStringList([]string{def}))
		versionRow.SetSelected(0)
		versionRow.SetSensitive(true)
		versionRow.SetSubtitle("Loading available releases…")
		go func() {
			ctx, cancel := context.WithTimeout(v.ctx, 15*time.Second)
			defer cancel()
			versions, err := backend.ListVersions(ctx, provider)
			glib.IdleAdd(func() {
				if v.selProvider != provider {
					return // selection changed while loading
				}
				if err != nil || len(versions) == 0 {
					versionRow.SetSubtitle("Could not fetch releases — using default")
					return
				}
				versionRow.SetModel(gtk.NewStringList(versions))
				versionRow.SetSelected(0)
				versionRow.SetSubtitle("")
			})
		}()
	}

	apply := func() {
		argoCheck.SetVisible(v.selProvider == backend.ProviderArgoCD)
		fluxCheck.SetVisible(v.selProvider == backend.ProviderFlux)
		setSelectedClass(argoRow, v.selProvider == backend.ProviderArgoCD)
		setSelectedClass(fluxRow, v.selProvider == backend.ProviderFlux)
		installBtn.SetSensitive(v.selProvider != backend.ProviderNone)
		switch v.selProvider {
		case backend.ProviderArgoCD:
			haRow.SetVisible(true)
			nsRow.SetSensitive(true)
			if nsRow.Text() == "" || nsRow.Text() == backend.FluxNamespace {
				nsRow.SetText(backend.ArgoCDNamespace)
			}
			loadVersions(backend.ProviderArgoCD)
		case backend.ProviderFlux:
			haRow.SetVisible(false)
			nsRow.SetText(backend.FluxNamespace)
			nsRow.SetSensitive(false)
			loadVersions(backend.ProviderFlux)
		}
	}

	argoRow.ConnectActivated(func() { v.selProvider = backend.ProviderArgoCD; apply() })
	fluxRow.ConnectActivated(func() { v.selProvider = backend.ProviderFlux; apply() })
	v.selProvider = backend.ProviderNone
	apply()

	installBtn.ConnectClicked(func() {
		version := ""
		if item := versionRow.SelectedItem(); item != nil {
			if so, ok := item.Cast().(*gtk.StringObject); ok {
				version = so.String()
			}
		}
		opts := backend.InstallOptions{
			Provider:         v.selProvider,
			Namespace:        nsRow.Text(),
			Version:          strings.TrimSpace(version),
			HighAvailability: haRow.Active(),
		}
		v.runInstall(opts)
	})

	return box
}

// runInstall switches to a progress screen and applies the manifests.
func (v *View) runInstall(opts backend.InstallOptions) {
	box, _ := shell("Installing " + opts.Provider.DisplayName())

	status := adw.NewStatusPage()
	status.SetVExpand(true)
	spinner := gtk.NewSpinner()
	spinner.SetSizeRequest(32, 32)
	spinner.Start()
	status.SetChild(spinner)
	status.SetTitle("Installing " + opts.Provider.DisplayName())
	status.SetDescription("Applying manifests to the cluster…")
	box.Append(status)

	// Log output.
	logBuf := gtk.NewTextView()
	logBuf.SetEditable(false)
	logBuf.SetMonospace(true)
	logBuf.AddCSSClass("card")
	logScroll := gtk.NewScrolledWindow()
	logScroll.SetChild(logBuf)
	logScroll.SetMarginStart(24)
	logScroll.SetMarginEnd(24)
	logScroll.SetMarginBottom(24)
	logScroll.SetMinContentHeight(160)
	logScroll.SetVExpand(false)
	box.Append(logScroll)

	v.setRoot(box)

	var logText strings.Builder
	appendLog := func(line string) {
		logText.WriteString(line)
		logText.WriteByte('\n')
		logBuf.Buffer().SetText(logText.String())
	}

	ctx, cancel := context.WithTimeout(v.ctx, 5*time.Minute)
	go func() {
		defer cancel()
		err := backend.Install(ctx, v.state.Cluster.Client, opts, func(line string) {
			glib.IdleAdd(func() { appendLog(line) })
		})
		glib.IdleAdd(func() {
			spinner.Stop()
			if err != nil {
				status.SetIconName("dialog-error-symbolic")
				status.SetTitle("Installation failed")
				status.SetDescription(err.Error())
				back := gtk.NewButtonWithLabel("Back")
				back.AddCSSClass("pill")
				back.SetHAlign(gtk.AlignCenter)
				back.ConnectClicked(func() { v.renderInstall() })
				status.SetChild(back)
				return
			}
			v.toast(opts.Provider.DisplayName() + " installed")
			v.refresh()
		})
	}()
}

// --- dashboard -----------------------------------------------------------

func (v *View) renderDashboard(provider backend.Provider, conflict bool) {
	ctx := v.newRenderCtx()

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("view")

	header := adw.NewHeaderBar()
	header.AddCSSClass("flat")
	header.SetTitleWidget(adw.NewWindowTitle("GitOps", ""))

	refreshBtn := gtk.NewButton()
	refreshBtn.SetIconName("update-symbolic")
	refreshBtn.SetTooltipText("Refresh")
	refreshBtn.AddCSSClass("flat")
	refreshBtn.ConnectClicked(func() { v.refresh() })
	header.PackEnd(refreshBtn)

	if provider == backend.ProviderArgoCD {
		newBtn := gtk.NewMenuButton()
		newBtn.SetIconName("plus-large-symbolic")
		newBtn.SetTooltipText("Create")
		newBtn.AddCSSClass("flat")
		newMenu := gio.NewMenu()
		newMenu.Append("New Application", "gitops.new-app")
		newMenu.Append("New Project", "gitops.new-project")
		newBtn.SetMenuModel(newMenu)
		header.PackEnd(newBtn)

		webBtn := gtk.NewButton()
		webBtn.SetIconName("globe-symbolic")
		webBtn.SetTooltipText("Open Web UI")
		webBtn.AddCSSClass("flat")
		webBtn.ConnectClicked(func() { v.openArgoWebUI(webBtn) })
		header.PackEnd(webBtn)
	} else {
		newBtn := gtk.NewButton()
		newBtn.SetIconName("plus-large-symbolic")
		newBtn.SetTooltipText("New Kustomization")
		newBtn.AddCSSClass("flat")
		newBtn.ConnectClicked(func() { v.presentFluxForm() })
		header.PackEnd(newBtn)
	}

	menuBtn := gtk.NewMenuButton()
	menuBtn.SetIconName("open-menu-symbolic")
	menuBtn.AddCSSClass("flat")
	menu := gio.NewMenu()
	menu.Append("Uninstall "+provider.DisplayName(), "gitops.uninstall")
	menuBtn.SetMenuModel(menu)
	header.PackEnd(menuBtn)

	actions := gio.NewSimpleActionGroup()
	uninstall := gio.NewSimpleAction("uninstall", nil)
	uninstall.ConnectActivate(func(_ *glib.Variant) { v.confirmUninstall(provider) })
	actions.AddAction(uninstall)
	if provider == backend.ProviderArgoCD {
		newApp := gio.NewSimpleAction("new-app", nil)
		newApp.ConnectActivate(func(_ *glib.Variant) { v.presentArgoForm() })
		actions.AddAction(newApp)
		newProject := gio.NewSimpleAction("new-project", nil)
		newProject.ConnectActivate(func(_ *glib.Variant) { v.presentArgoProjectForm() })
		actions.AddAction(newProject)
	}
	box.InsertActionGroup("gitops", actions)

	box.Append(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	box.Append(scroll)

	if conflict {
		warn := adw.NewPreferencesGroup()
		warn.SetTitle("Multiple engines detected")
		warn.SetDescription("Both Argo CD and Flux CD appear to be installed. Running both is unsupported; consider uninstalling one.")
		page.Add(warn)
	}

	// Overview.
	overview := adw.NewPreferencesGroup()
	overview.SetTitle("Overview")
	engineRow := adw.NewActionRow()
	engineRow.SetTitle("Engine")
	engineRow.SetSubtitle(provider.DisplayName())
	overview.Add(engineRow)
	nsRow := adw.NewActionRow()
	nsRow.SetTitle("Namespace")
	nsRow.SetSubtitle(namespaceFor(provider))
	overview.Add(nsRow)
	controllersRow := adw.NewActionRow()
	controllersRow.SetTitle("Controllers")
	controllersRow.SetSubtitle("…")
	overview.Add(controllersRow)
	page.Add(overview)
	v.watchControllers(ctx, namespaceFor(provider), controllersRow)

	if provider == backend.ProviderArgoCD {
		v.buildArgoGroups(ctx, page)
	} else {
		v.buildFluxGroups(ctx, page)
	}

	v.SetChild(box)
}

func namespaceFor(p backend.Provider) string {
	if p == backend.ProviderArgoCD {
		return backend.ArgoCDNamespace
	}
	return backend.FluxNamespace
}

// openArgoWebUI presents a dialog showing the Argo CD admin credentials and
// lets the user reveal/copy the password before opening the web UI in their
// browser (which requires a port-forward to the argocd-server pod).
func (v *View) openArgoWebUI(btn *gtk.Button) {
	btn.SetSensitive(false)
	go func() {
		ctx, cancel := context.WithTimeout(v.ctx, 15*time.Second)
		defer cancel()
		password, pwErr := backend.ArgoInitialAdminPassword(ctx, v.state.Cluster.Clientset, backend.ArgoCDNamespace)
		glib.IdleAdd(func() {
			btn.SetSensitive(true)
			v.presentArgoCredentials(password, pwErr)
		})
	}()
}

// presentArgoCredentials builds the credentials dialog. password may be empty
// when pwErr is non-nil (e.g. the initial secret was rotated/removed).
func (v *View) presentArgoCredentials(password string, pwErr error) {
	dialog := adw.NewDialog()
	dialog.SetTitle("Argo CD Login")
	dialog.SetContentWidth(440)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	header := adw.NewHeaderBar()
	box.Append(header)

	page := adw.NewPreferencesPage()
	box.Append(page)

	group := adw.NewPreferencesGroup()
	group.SetTitle("Administrator Credentials")
	group.SetDescription("Use these to sign in to the Argo CD web interface.")
	page.Add(group)

	clipboard := func(text string) {
		if d := gdk.DisplayGetDefault(); d != nil {
			d.Clipboard().SetText(text)
		}
		v.toast("Copied to clipboard")
	}

	// Username row.
	userRow := adw.NewActionRow()
	userRow.SetTitle("Username")
	userRow.SetSubtitle(backend.ArgoAdminUsername)
	userRow.SetSubtitleSelectable(true)
	copyUser := gtk.NewButtonFromIconName("edit-copy-symbolic")
	copyUser.SetTooltipText("Copy username")
	copyUser.AddCSSClass("flat")
	copyUser.SetVAlign(gtk.AlignCenter)
	copyUser.ConnectClicked(func() { clipboard(backend.ArgoAdminUsername) })
	userRow.AddSuffix(copyUser)
	group.Add(userRow)

	// Password row.
	passRow := adw.NewActionRow()
	passRow.SetTitle("Password")
	passRow.SetSubtitleSelectable(true)
	group.Add(passRow)

	openBtn := gtk.NewButtonWithLabel("Open in Browser")
	openBtn.AddCSSClass("suggested-action")

	if pwErr != nil {
		passRow.SetSubtitle("Unavailable — set your own password")
		passRow.AddCSSClass("dim-label")
	} else {
		const dots = "••••••••••••"
		passRow.SetSubtitle(dots)

		revealed := false
		reveal := gtk.NewButtonFromIconName("view-reveal-symbolic")
		reveal.SetTooltipText("Show password")
		reveal.AddCSSClass("flat")
		reveal.SetVAlign(gtk.AlignCenter)
		reveal.ConnectClicked(func() {
			revealed = !revealed
			if revealed {
				passRow.SetSubtitle(password)
				reveal.SetIconName("view-conceal-symbolic")
				reveal.SetTooltipText("Hide password")
			} else {
				passRow.SetSubtitle(dots)
				reveal.SetIconName("view-reveal-symbolic")
				reveal.SetTooltipText("Show password")
			}
		})
		passRow.AddSuffix(reveal)

		copyPass := gtk.NewButtonFromIconName("edit-copy-symbolic")
		copyPass.SetTooltipText("Copy password")
		copyPass.AddCSSClass("flat")
		copyPass.SetVAlign(gtk.AlignCenter)
		copyPass.ConnectClicked(func() { clipboard(password) })
		passRow.AddSuffix(copyPass)
	}

	// Footer action.
	openBtn.SetMarginStart(12)
	openBtn.SetMarginEnd(12)
	openBtn.SetMarginTop(6)
	openBtn.SetMarginBottom(12)
	openBtn.ConnectClicked(func() {
		dialog.Close()
		v.launchArgoWebUI()
	})
	box.Append(openBtn)

	dialog.SetChild(box)
	dialog.Present(v)
}

// launchArgoWebUI establishes (once) a port-forward to argocd-server and opens
// the resulting local URL in the browser.
func (v *View) launchArgoWebUI() {
	if v.argoWebURL != "" {
		v.openURL(v.argoWebURL)
		return
	}
	v.toast("Connecting to Argo CD…")
	go func() {
		url, err := backend.StartArgoServerForward(v.ctx, v.state.Cluster.Config, v.state.Cluster.Clientset, backend.ArgoCDNamespace)
		glib.IdleAdd(func() {
			if err != nil {
				widget.ShowErrorDialog(v.ctx, "Could not open Argo CD web UI", err)
				return
			}
			v.argoWebURL = url
			v.openURL(url)
		})
	}()
}

// openURL launches the given URL in the user's default browser.
func (v *View) openURL(url string) {
	var window *gtk.Window
	if w, ok := ctxt.From[*gtk.Window](v.ctx); ok {
		window = w
	}
	gtk.ShowURI(window, url, 0)
}

// watchControllers updates a row with a live ready/total pod count.
func (v *View) watchControllers(ctx context.Context, namespace string, row *adw.ActionRow) {
	pods := pubsub.NewProperty[[]client.Object](nil)
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	if err := api.InformerConnectProperty(ctx, v.state.Cluster, gvr, pods); err != nil {
		row.SetSubtitle("unavailable")
		return
	}
	pods.Sub(ctx, func(objs []client.Object) {
		ready, total := 0, 0
		for _, o := range objs {
			if o.GetNamespace() != namespace {
				continue
			}
			total++
			u, ok := o.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
			if phase == "Running" || phase == "Succeeded" {
				ready++
			}
		}
		if total == 0 {
			row.SetSubtitle("no pods")
		} else {
			row.SetSubtitle(fmt.Sprintf("%d/%d running", ready, total))
		}
	})
}

// --- resource group helper ----------------------------------------------

type resourceGroup struct {
	group *adw.PreferencesGroup
	rows  []gtk.Widgetter
	empty *adw.ActionRow
}

func newResourceGroup(title, description string) *resourceGroup {
	g := adw.NewPreferencesGroup()
	g.SetTitle(title)
	if description != "" {
		g.SetDescription(description)
	}
	return &resourceGroup{group: g}
}

func (rg *resourceGroup) clear() {
	for _, r := range rg.rows {
		rg.group.Remove(r)
	}
	rg.rows = nil
	if rg.empty != nil {
		rg.group.Remove(rg.empty)
		rg.empty = nil
	}
}

func (rg *resourceGroup) add(row gtk.Widgetter) {
	rg.group.Add(row)
	rg.rows = append(rg.rows, row)
}

func (rg *resourceGroup) setEmpty(msg string) {
	rg.empty = adw.NewActionRow()
	rg.empty.SetTitle(msg)
	rg.empty.AddCSSClass("dim-label")
	rg.group.Add(rg.empty)
}

// connect wires a watched GVR to a rebuild callback (debounced, main thread).
func (v *View) connect(ctx context.Context, gvr schema.GroupVersionResource, rebuild func([]client.Object)) {
	prop := pubsub.NewProperty[[]client.Object](nil)
	if err := api.InformerConnectProperty(ctx, v.state.Cluster, gvr, prop); err != nil {
		return
	}
	deb, _ := debounce.Debounce(func() {
		glib.IdleAdd(func() { rebuild(prop.Value()) })
	}, 150*time.Millisecond)
	prop.Sub(ctx, func([]client.Object) { deb() })
}

func sortByName(objs []client.Object) {
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].GetName() < objs[j].GetName() })
}

func statusPill(text, cssClass string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.AddCSSClass("caption")
	l.AddCSSClass("pill")
	if cssClass != "" {
		l.AddCSSClass(cssClass)
	}
	l.SetVAlign(gtk.AlignCenter)
	return l
}

func actionButton(icon, tooltip string, fn func()) *gtk.Button {
	b := gtk.NewButton()
	b.SetIconName(icon)
	b.SetTooltipText(tooltip)
	b.AddCSSClass("flat")
	b.SetVAlign(gtk.AlignCenter)
	b.ConnectClicked(func() { fn() })
	return b
}

// --- Argo CD groups ------------------------------------------------------

func (v *View) buildArgoGroups(ctx context.Context, page *adw.PreferencesPage) {
	apps := newResourceGroup("Applications", "")
	page.Add(apps.group)
	v.connect(ctx, backend.GVRApplication, func(objs []client.Object) {
		apps.clear()
		sortByName(objs)
		if len(objs) == 0 {
			apps.setEmpty("No applications yet")
			return
		}
		for _, o := range objs {
			apps.add(v.argoAppRow(o))
		}
	})

	projects := newResourceGroup("Projects", "")
	page.Add(projects.group)
	v.connect(ctx, backend.GVRAppProject, func(objs []client.Object) {
		projects.clear()
		sortByName(objs)
		if len(objs) == 0 {
			projects.setEmpty("No projects")
			return
		}
		for _, o := range objs {
			row := adw.NewActionRow()
			row.SetTitle(o.GetName())
			row.AddPrefix(gtk.NewImageFromIconName("folder-symbolic"))
			projects.add(row)
		}
	})
}

func (v *View) argoAppRow(o client.Object) gtk.Widgetter {
	st := backend.ReadArgoStatus(o)
	repo, path, rev, destNS := backend.ArgoSource(o)

	row := adw.NewExpanderRow()
	row.SetTitle(o.GetName())
	subtitle := repo
	if path != "" {
		subtitle = strings.TrimSuffix(repo, ".git") + " · " + path
	}
	row.SetSubtitle(subtitle)
	row.AddPrefix(gtk.NewImageFromIconName("globe-symbolic"))

	if st.Health != "" {
		row.AddSuffix(statusPill(st.Health, st.HealthClass().CSSClass()))
	}
	if st.Sync != "" {
		cls := "warning"
		if st.Sync == "Synced" {
			cls = "success"
		}
		row.AddSuffix(statusPill(st.Sync, cls))
	}

	ns, name := o.GetNamespace(), o.GetName()
	row.AddSuffix(actionButton("update-symbolic", "Sync", func() {
		v.do("Syncing "+name, func(ctx context.Context) error {
			return backend.SyncApplication(ctx, v.state.Cluster.Client, ns, name, false, false)
		})
	}))
	row.AddSuffix(actionButton("view-refresh-symbolic", "Refresh", func() {
		v.do("Refreshing "+name, func(ctx context.Context) error {
			return backend.RefreshApplication(ctx, v.state.Cluster.Client, ns, name, "normal")
		})
	}))
	row.AddSuffix(actionButton("text-editor-symbolic", "Edit", func() { v.edit(o) }))
	row.AddSuffix(actionButton("user-trash-symbolic", "Delete", func() { v.confirmDelete(o) }))

	addInfoRow(row, "Target Revision", emptyDash(rev))
	addInfoRow(row, "Synced Revision", emptyDash(st.Revision))
	addInfoRow(row, "Destination Namespace", emptyDash(destNS))
	return row
}

// --- Flux groups ---------------------------------------------------------

func (v *View) buildFluxGroups(ctx context.Context, page *adw.PreferencesPage) {
	kust := newResourceGroup("Kustomizations", "")
	page.Add(kust.group)
	v.connect(ctx, backend.GVRKustomization, func(objs []client.Object) {
		kust.clear()
		sortByName(objs)
		if len(objs) == 0 {
			kust.setEmpty("No kustomizations yet")
			return
		}
		for _, o := range objs {
			kust.add(v.fluxRow(o, backend.GVRKustomization.GroupVersion().WithKind("Kustomization"), "package-x-generic-symbolic", true))
		}
	})

	sources := newResourceGroup("Sources", "")
	page.Add(sources.group)
	v.connect(ctx, backend.GVRGitRepository, func(objs []client.Object) {
		sources.clear()
		sortByName(objs)
		if len(objs) == 0 {
			sources.setEmpty("No Git repositories")
			return
		}
		for _, o := range objs {
			sources.add(v.fluxRow(o, backend.GVRGitRepository.GroupVersion().WithKind("GitRepository"), "git-symbolic", false))
		}
	})

	helm := newResourceGroup("Helm Releases", "")
	page.Add(helm.group)
	v.connect(ctx, backend.GVRHelmRelease, func(objs []client.Object) {
		helm.clear()
		sortByName(objs)
		if len(objs) == 0 {
			helm.setEmpty("No Helm releases")
			return
		}
		for _, o := range objs {
			helm.add(v.fluxRow(o, backend.GVRHelmRelease.GroupVersion().WithKind("HelmRelease"), "library-symbolic", true))
		}
	})
}

func (v *View) fluxRow(o client.Object, gvk schema.GroupVersionKind, icon string, suspendable bool) gtk.Widgetter {
	st := backend.ReadFluxStatus(o)
	row := adw.NewExpanderRow()
	row.SetTitle(o.GetName())
	if url, branch := backend.FluxGitSource(o); url != "" {
		if branch != "" {
			row.SetSubtitle(strings.TrimSuffix(url, ".git") + " @ " + branch)
		} else {
			row.SetSubtitle(url)
		}
	}
	row.AddPrefix(gtk.NewImageFromIconName(icon))
	row.AddSuffix(statusPill(st.ReadyLabel(), st.HealthClass().CSSClass()))

	ns, name := o.GetNamespace(), o.GetName()
	row.AddSuffix(actionButton("update-symbolic", "Reconcile", func() {
		v.do("Reconciling "+name, func(ctx context.Context) error {
			return backend.Reconcile(ctx, v.state.Cluster.Client, gvk, ns, name)
		})
	}))
	if suspendable {
		if st.Suspended {
			row.AddSuffix(actionButton("play-symbolic", "Resume", func() {
				v.do("Resuming "+name, func(ctx context.Context) error {
					return backend.SetSuspend(ctx, v.state.Cluster.Client, gvk, ns, name, false)
				})
			}))
		} else {
			row.AddSuffix(actionButton("stop-symbolic", "Suspend", func() {
				v.do("Suspending "+name, func(ctx context.Context) error {
					return backend.SetSuspend(ctx, v.state.Cluster.Client, gvk, ns, name, true)
				})
			}))
		}
	}
	row.AddSuffix(actionButton("text-editor-symbolic", "Edit", func() { v.edit(o) }))
	row.AddSuffix(actionButton("user-trash-symbolic", "Delete", func() { v.confirmDelete(o) }))

	addInfoRow(row, "Applied Revision", emptyDash(st.Revision))
	if st.Message != "" {
		addInfoRow(row, "Message", st.Message)
	}
	return row
}

// --- shared row + action helpers ----------------------------------------

func addInfoRow(exp *adw.ExpanderRow, title, value string) {
	r := adw.NewActionRow()
	r.SetTitle(title)
	r.SetSubtitle(value)
	r.SetSubtitleSelectable(true)
	r.AddCSSClass("property")
	exp.AddRow(r)
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// do runs a backend action asynchronously and toasts the result.
func (v *View) do(msg string, fn func(context.Context) error) {
	v.toast(msg)
	go func() {
		ctx, cancel := context.WithTimeout(v.ctx, 30*time.Second)
		defer cancel()
		err := fn(ctx)
		glib.IdleAdd(func() {
			if err != nil {
				widget.ShowErrorDialog(v.ctx, "Action failed", err)
			}
		})
	}()
}

func (v *View) edit(o client.Object) {
	gvk := o.GetObjectKind().GroupVersionKind()
	if err := v.editor.AddPage(&gvk, o); err != nil {
		widget.ShowErrorDialog(v.ctx, "Error loading editor", err)
		return
	}
	v.editor.Present()
}

func (v *View) confirmDelete(o client.Object) {
	dialog := adw.NewAlertDialog(
		fmt.Sprintf("Delete %s?", o.GetObjectKind().GroupVersionKind().Kind),
		o.GetName())
	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("delete", "Delete")
	dialog.SetResponseAppearance("delete", adw.ResponseDestructive)
	dialog.ConnectResponse(func(response string) {
		if response != "delete" {
			return
		}
		name := o.GetName()
		v.do("Deleting "+name, func(ctx context.Context) error {
			return v.state.Cluster.Delete(ctx, o)
		})
	})
	dialog.Present(v)
}

func (v *View) confirmUninstall(provider backend.Provider) {
	dialog := adw.NewAlertDialog(
		"Uninstall "+provider.DisplayName()+"?",
		"This deletes the engine's namespace and custom resource definitions, removing all managed GitOps resources. This cannot be undone.")
	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("uninstall", "Uninstall")
	dialog.SetResponseAppearance("uninstall", adw.ResponseDestructive)
	dialog.ConnectResponse(func(response string) {
		if response != "uninstall" {
			return
		}
		v.setRoot(v.loadingScreen("Uninstalling " + provider.DisplayName() + "…"))
		go func() {
			ctx, cancel := context.WithTimeout(v.ctx, 3*time.Minute)
			defer cancel()
			err := backend.Uninstall(ctx, v.state.Cluster.Client, provider, nil)
			glib.IdleAdd(func() {
				if err != nil {
					v.setRoot(v.errorScreen("Uninstall failed", err, v.refresh))
					return
				}
				v.toast(provider.DisplayName() + " uninstalled")
				v.refresh()
			})
		}()
	})
	dialog.Present(v)
}

// --- create forms --------------------------------------------------------

func (v *View) presentArgoForm() {
	dialog := adw.NewDialog()
	dialog.SetTitle("New Application")
	dialog.SetContentWidth(520)
	dialog.SetContentHeight(640)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	header := adw.NewHeaderBar()
	create := gtk.NewButtonWithLabel("Create")
	create.AddCSSClass("suggested-action")
	header.PackEnd(create)
	box.Append(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	box.Append(scroll)

	g := adw.NewPreferencesGroup()
	g.SetTitle("Application")
	page.Add(g)
	name := entry(g, "Name")
	project := entry(g, "Project")
	project.SetText("default")

	src := adw.NewPreferencesGroup()
	src.SetTitle("Source")
	page.Add(src)
	repo := entry(src, "Repository URL")
	path := entry(src, "Path")
	rev := entry(src, "Target Revision")
	rev.SetText("HEAD")

	dst := adw.NewPreferencesGroup()
	dst.SetTitle("Destination")
	page.Add(dst)
	destNS := entry(dst, "Namespace")
	destNS.SetText("default")

	pol := adw.NewPreferencesGroup()
	pol.SetTitle("Sync Policy")
	page.Add(pol)
	auto := switchRow(pol, "Automated Sync", "Automatically sync when the repository changes")
	selfHeal := switchRow(pol, "Self Heal", "Revert manual changes that drift from Git")
	prune := switchRow(pol, "Prune", "Delete resources removed from Git")
	createNS := switchRow(pol, "Create Namespace", "Create the destination namespace if missing")

	dialog.SetChild(box)
	dialog.Present(v)

	create.ConnectClicked(func() {
		if strings.TrimSpace(name.Text()) == "" || strings.TrimSpace(repo.Text()) == "" {
			v.toast("Name and Repository URL are required")
			return
		}
		spec := backend.ArgoApplicationSpec{
			Name:            strings.TrimSpace(name.Text()),
			Namespace:       backend.ArgoCDNamespace,
			Project:         strings.TrimSpace(project.Text()),
			RepoURL:         strings.TrimSpace(repo.Text()),
			Path:            strings.TrimSpace(path.Text()),
			TargetRevision:  strings.TrimSpace(rev.Text()),
			DestNamespace:   strings.TrimSpace(destNS.Text()),
			AutoSync:        auto.Active(),
			SelfHeal:        selfHeal.Active(),
			Prune:           prune.Active(),
			CreateNamespace: createNS.Active(),
		}
		obj := backend.BuildArgoApplication(spec)
		v.createObjects(dialog, "Application created", []*unstructured.Unstructured{obj})
	})
}

func (v *View) presentArgoProjectForm() {
	dialog := adw.NewDialog()
	dialog.SetTitle("New Project")
	dialog.SetContentWidth(520)
	dialog.SetContentHeight(560)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	header := adw.NewHeaderBar()
	create := gtk.NewButtonWithLabel("Create")
	create.AddCSSClass("suggested-action")
	header.PackEnd(create)
	box.Append(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	box.Append(scroll)

	g := adw.NewPreferencesGroup()
	g.SetTitle("Project")
	page.Add(g)
	name := entry(g, "Name")
	description := entry(g, "Description")

	src := adw.NewPreferencesGroup()
	src.SetTitle("Source Repositories")
	src.SetDescription("Comma-separated repository URLs. Use * to allow any.")
	page.Add(src)
	repos := entry(src, "Allowed Repositories")
	repos.SetText("*")

	dst := adw.NewPreferencesGroup()
	dst.SetTitle("Destination")
	page.Add(dst)
	destNS := entry(dst, "Allowed Namespace")
	destNS.SetText("*")

	perm := adw.NewPreferencesGroup()
	perm.SetTitle("Permissions")
	page.Add(perm)
	clusterRes := switchRow(perm, "Allow Cluster Resources", "Permit applications to manage cluster-scoped resources")
	nsRes := switchRow(perm, "Allow Namespace Resources", "Permit applications to manage namespaced resources")
	nsRes.SetActive(true)

	dialog.SetChild(box)
	dialog.Present(v)

	create.ConnectClicked(func() {
		if strings.TrimSpace(name.Text()) == "" {
			v.toast("Name is required")
			return
		}
		var sourceRepos []string
		for _, r := range strings.Split(repos.Text(), ",") {
			if r = strings.TrimSpace(r); r != "" {
				sourceRepos = append(sourceRepos, r)
			}
		}
		spec := backend.ArgoProjectSpec{
			Name:                       strings.TrimSpace(name.Text()),
			Namespace:                  backend.ArgoCDNamespace,
			Description:                strings.TrimSpace(description.Text()),
			SourceRepos:                sourceRepos,
			DestNS:                     strings.TrimSpace(destNS.Text()),
			AllowAllClusterResources:   clusterRes.Active(),
			AllowAllNamespaceResources: nsRes.Active(),
		}
		obj := backend.BuildArgoAppProject(spec)
		v.createObjects(dialog, "Project created", []*unstructured.Unstructured{obj})
	})
}

func (v *View) presentFluxForm() {
	dialog := adw.NewDialog()
	dialog.SetTitle("New Kustomization")
	dialog.SetContentWidth(520)
	dialog.SetContentHeight(620)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	header := adw.NewHeaderBar()
	create := gtk.NewButtonWithLabel("Create")
	create.AddCSSClass("suggested-action")
	header.PackEnd(create)
	box.Append(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	box.Append(scroll)

	g := adw.NewPreferencesGroup()
	g.SetTitle("Kustomization")
	page.Add(g)
	name := entry(g, "Name")
	interval := entry(g, "Interval")
	interval.SetText("5m")
	path := entry(g, "Path")
	path.SetText("./")
	targetNS := entry(g, "Target Namespace")

	src := adw.NewPreferencesGroup()
	src.SetTitle("Git Source")
	page.Add(src)
	createSrc := switchRow(src, "Create Git Repository", "Create a GitRepository source alongside the Kustomization")
	createSrc.SetActive(true)
	repo := entry(src, "Repository URL")
	branch := entry(src, "Branch")
	branch.SetText("main")

	pol := adw.NewPreferencesGroup()
	pol.SetTitle("Options")
	page.Add(pol)
	prune := switchRow(pol, "Prune", "Garbage-collect resources removed from Git")
	prune.SetActive(true)

	dialog.SetChild(box)
	dialog.Present(v)

	create.ConnectClicked(func() {
		if strings.TrimSpace(name.Text()) == "" || strings.TrimSpace(repo.Text()) == "" {
			v.toast("Name and Repository URL are required")
			return
		}
		spec := backend.FluxKustomizationSpec{
			Name:         strings.TrimSpace(name.Text()),
			Namespace:    backend.FluxNamespace,
			RepoURL:      strings.TrimSpace(repo.Text()),
			Branch:       strings.TrimSpace(branch.Text()),
			Path:         strings.TrimSpace(path.Text()),
			Interval:     strings.TrimSpace(interval.Text()),
			Prune:        prune.Active(),
			TargetNS:     strings.TrimSpace(targetNS.Text()),
			CreateSource: createSrc.Active(),
		}
		objs := backend.BuildFluxObjects(spec)
		v.createObjects(dialog, "Kustomization created", objs)
	})
}

func (v *View) createObjects(dialog *adw.Dialog, successMsg string, objs []*unstructured.Unstructured) {
	go func() {
		ctx, cancel := context.WithTimeout(v.ctx, 30*time.Second)
		defer cancel()
		var err error
		for _, o := range objs {
			if err = backend.Create(ctx, v.state.Cluster.Client, o); err != nil {
				break
			}
		}
		glib.IdleAdd(func() {
			if err != nil {
				widget.ShowErrorDialog(v.ctx, "Could not create resource", err)
				return
			}
			dialog.Close()
			v.toast(successMsg)
		})
	}()
}

func entry(g *adw.PreferencesGroup, title string) *adw.EntryRow {
	e := adw.NewEntryRow()
	e.SetTitle(title)
	g.Add(e)
	return e
}

func switchRow(g *adw.PreferencesGroup, title, subtitle string) *adw.SwitchRow {
	s := adw.NewSwitchRow()
	s.SetTitle(title)
	if subtitle != "" {
		s.SetSubtitle(subtitle)
	}
	g.Add(s)
	return s
}

// keep pango import for potential ellipsization tweaks
var _ = pango.EllipsizeEnd
