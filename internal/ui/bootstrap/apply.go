package bootstrap

import (
	"context"
	"fmt"
	"html"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/SilkePilon/Orchestrator/internal/bootstrap"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	ansi "github.com/leaanthony/go-ansi-parser"
)

// applyPage renders the live execution view: a TabView with one tab per
// node containing a streaming log, plus a sticky bottom bar showing
// overall progress and an Abort button. When Run finishes, the wizard
// pushes the Finish page automatically.
func (w *Wizard) applyPage() *adw.NavigationPage {
	d := w.draft.Value()
	if d.Plan == nil {
		w.errorToast(fmt.Errorf("no plan to apply"))
		return w.pageShell("Apply", "", gtk.NewLabel(""), nil)
	}

	ctx, cancel := context.WithCancel(w.ctx)
	aborted := &atomic.Bool{}

	// Abort button sits in the header bar — GNOME HIG places destructive
	// actions in the header, not the content area.
	abort := gtk.NewButtonWithLabel("Abort")
	abort.AddCSSClass("destructive-action")
	abort.ConnectClicked(func() {
		aborted.Store(true)
		abort.SetSensitive(false)
		abort.SetLabel("Aborting…")
		cancel()
	})

	// WindowTitle lets us update both title and subtitle as execution progresses.
	titleWidget := adw.NewWindowTitle("Applying…", "")

	// Progress bar lives in a bottom ActionBar for the correct GNOME
	// bottom-toolbar appearance (separator line, extend-background colour).
	progress := gtk.NewProgressBar()
	progress.SetHExpand(true)
	progress.SetMarginStart(6)
	progress.SetMarginEnd(6)

	actionBar := gtk.NewActionBar()
	actionBar.SetCenterWidget(progress)

	// Tab view — one tab per node.
	tabView := adw.NewTabView()
	tabBar := adw.NewTabBar()
	tabBar.SetView(tabView)

	logs := map[string]*nodeLog{}
	tabPages := map[string]*adw.TabPage{}
	for _, nodeID := range d.Plan.NodeOrder {
		var node core.Node
		for _, n := range d.Nodes {
			if n.ID == nodeID {
				node = n
				break
			}
		}
		nl := newNodeLog(d.Plan.NodeSteps[nodeID])
		logs[nodeID] = nl
		page := tabView.Append(nl.widget())
		page.SetTitle(labelOr(node))
		tabPages[nodeID] = page
	}

	// ToolbarView is the modern Adwaita way to place toolbars above and
	// below scrollable content: it draws the correct shadow/separator
	// lines and applies extend-background colouring automatically.
	toolbar := adw.NewToolbarView()
	if len(d.Plan.NodeOrder) > 1 {
		toolbar.AddTopBar(tabBar)
	}
	toolbar.SetContent(tabView)
	toolbar.AddBottomBar(actionBar)

	// Build the page manually so the Abort button goes into the header
	// bar end slot instead of the content area.
	pageBox := gtk.NewBox(gtk.OrientationVertical, 0)
	navPage := adw.NewNavigationPage(pageBox, "Applying…")

	header := adw.NewHeaderBar()
	header.SetTitleWidget(titleWidget)
	header.PackEnd(abort)
	pageBox.Append(header)
	pageBox.Append(toolbar)

	go w.runApply(ctx, cancel, aborted, logs, tabView, tabPages, progress, abort, titleWidget)

	return navPage
}

func (w *Wizard) runApply(
	ctx context.Context,
	cancel context.CancelFunc,
	aborted *atomic.Bool,
	logs map[string]*nodeLog,
	tabView *adw.TabView,
	tabPages map[string]*adw.TabPage,
	progress *gtk.ProgressBar,
	abort *gtk.Button,
	titleWidget *adw.WindowTitle,
) {
	d := w.draft.Value()
	store, err := core.DefaultKnownHosts()
	if err != nil {
		glib.IdleAdd(func() {
			w.push(w.finishPage(false, "", err))
		})
		return
	}

	// Dial all nodes up-front so a failure shows quickly.
	clients := map[string]*core.Client{}
	for _, n := range d.Nodes {
		c, derr := core.Dial(ctx, n, store, w.makeHostKeyPrompt())
		if derr != nil {
			glib.IdleAdd(func() {
				w.push(w.finishPage(false, "", fmt.Errorf("connect to %s: %w", labelOr(n), derr)))
			})
			return
		}
		clients[n.ID] = c
	}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	exec := core.NewExecutor(d.Plan, clients)

	totalSteps := 0
	for _, steps := range d.Plan.NodeSteps {
		totalSteps += len(steps)
	}
	doneSteps := 0

	// Drain events on this goroutine and dispatch UI mutations.
	doneCh := make(chan error, 1)
	go func() { doneCh <- exec.Run(ctx) }()

	for ev := range exec.Events() {
		ev := ev
		glib.IdleAdd(func() {
			if ev.Kind == "step.start" {
				if page, ok := tabPages[ev.NodeID]; ok {
					tabView.SetSelectedPage(page)
				}
			}
			if nl, ok := logs[ev.NodeID]; ok {
				nl.handle(ev)
			}
			if ev.Kind == "step.end" {
				doneSteps++
				progress.SetFraction(float64(doneSteps) / float64(totalSteps))
			}
		})
	}

	runErr := <-doneCh
	if !aborted.Load() {
		cancel()
	}

	// Did any step fail?
	finalErr := runErr
	if aborted.Load() {
		finalErr = context.Canceled
	}
	for _, nl := range logs {
		if nl.failed {
			if finalErr == nil {
				finalErr = fmt.Errorf("one or more steps failed")
			}
		}
	}

	kubeconfig := exec.KubeconfigYAML()
	success := !aborted.Load() && finalErr == nil && (!w.requireKubeconfig || kubeconfig != "")

	glib.IdleAdd(func() {
		abort.SetSensitive(false)
		if success {
			progress.SetFraction(1)
			progress.SetText("All steps completed")
			titleWidget.SetTitle("Done")
			titleWidget.SetSubtitle("Bootstrap completed successfully")
		} else if aborted.Load() {
			progress.SetText("Canceled")
			titleWidget.SetTitle("Canceled")
			titleWidget.SetSubtitle("")
		} else {
			progress.SetText("Failed")
			titleWidget.SetTitle("Failed")
			titleWidget.SetSubtitle("One or more steps did not complete")
		}
		w.push(w.finishPage(success, kubeconfig, finalErr))
	})
}

// nodeLog is the per-tab UI: a sidebar of step rows + a streaming log
// pane on the right.
type nodeLog struct {
	pane    *gtk.Paned
	stepBox *gtk.ListBox
	steps   map[string]*stepStatusRow
	view    *gtk.TextView
	buf     *gtk.TextBuffer
	mu      sync.Mutex
	failed  bool
}

func newNodeLog(steps []core.Step) *nodeLog {
	pane := gtk.NewPaned(gtk.OrientationHorizontal)

	// Left side: step list styled as a navigation sidebar.
	listBox := gtk.NewListBox()
	listBox.AddCSSClass("navigation-sidebar")
	listBox.SetSelectionMode(gtk.SelectionNone)

	sidebarScroll := gtk.NewScrolledWindow()
	sidebarScroll.SetChild(listBox)
	sidebarScroll.SetSizeRequest(290, -1)
	sidebarScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	pane.SetStartChild(sidebarScroll)
	pane.SetResizeStartChild(false)

	// Right side: monospace log output.
	view := gtk.NewTextView()
	view.SetMonospace(true)
	view.SetEditable(false)
	view.SetCursorVisible(false)
	view.SetWrapMode(gtk.WrapWordChar)
	view.SetLeftMargin(10)
	view.SetRightMargin(10)
	view.SetTopMargin(8)
	view.SetBottomMargin(8)

	logScroll := gtk.NewScrolledWindow()
	logScroll.SetVExpand(true)
	logScroll.SetHExpand(true)
	logScroll.SetChild(view)
	pane.SetEndChild(logScroll)
	pane.SetResizeEndChild(true)

	nl := &nodeLog{
		pane:    pane,
		stepBox: listBox,
		steps:   map[string]*stepStatusRow{},
		view:    view,
		buf:     view.Buffer(),
	}
	for _, st := range steps {
		row := newStepStatusRow(st)
		nl.steps[st.ID] = row
		listBox.Append(row.widget())
	}
	return nl
}

func (nl *nodeLog) widget() gtk.Widgetter { return nl.pane }

func (nl *nodeLog) handle(ev core.Event) {
	nl.mu.Lock()
	defer nl.mu.Unlock()
	switch ev.Kind {
	case "step.start":
		if r, ok := nl.steps[ev.StepID]; ok {
			r.setStatus(core.StatusRunning, 0, "")
		}
	case "step.end":
		detail := ""
		if ev.Status == core.StatusFailed {
			detail = fmt.Sprintf("Exited with code %d", ev.ExitCode)
			nl.failed = true
			nl.appendLine(fmt.Sprintf("✗ step failed (exit %d): %v", ev.ExitCode, ev.Err))
		}
		if r, ok := nl.steps[ev.StepID]; ok {
			r.setStatus(ev.Status, ev.ExitCode, detail)
		}
	case "stdout", "stderr", "log":
		nl.appendLine(ev.Line)
	}
}

func (nl *nodeLog) appendLine(line string) {
	stamp := time.Now().Format("15:04:05")
	prefix := fmt.Sprintf("[%s] ", stamp)
	nl.buf.Insert(nl.buf.EndIter(), prefix)

	segments, err := ansi.Parse(line)
	if err != nil || len(segments) == 0 {
		nl.buf.Insert(nl.buf.EndIter(), line+"\n")
	} else {
		for _, seg := range segments {
			var attrs []string
			if seg.FgCol != nil {
				attrs = append(attrs, fmt.Sprintf(`foreground=%q`, seg.FgCol.Hex))
			}
			if seg.BgCol != nil {
				attrs = append(attrs, fmt.Sprintf(`background=%q`, seg.BgCol.Hex))
			}
			if len(attrs) > 0 {
				nl.buf.InsertMarkup(nl.buf.EndIter(), fmt.Sprintf(`<span %s>%s</span>`, strings.Join(attrs, " "), html.EscapeString(seg.Label)))
			} else {
				nl.buf.Insert(nl.buf.EndIter(), seg.Label)
			}
		}
		nl.buf.Insert(nl.buf.EndIter(), "\n")
	}

	// Autoscroll: scroll the view to the end mark.
	mark := nl.buf.CreateMark("end", nl.buf.EndIter(), false)
	nl.view.ScrollMarkOnscreen(mark)
	nl.buf.DeleteMark(mark)
}

// stepStatusRow is one entry in the step sidebar, built on adw.ActionRow
// for proper GNOME styling: hover highlight, title+subtitle typography,
// and a prefix slot for the status indicator.
type stepStatusRow struct {
	row         *adw.ActionRow
	indicator   *adw.Bin
	icon        *gtk.Image
	spinner     *gtk.Spinner
	description string // original description to restore after status changes
}

func newStepStatusRow(st core.Step) *stepStatusRow {
	row := adw.NewActionRow()
	row.SetTitle(st.Title)

	// Subtitle: the step description, overridden by the skip reason when
	// the step is pre-skipped by the plan generator.
	subtitle := st.Description
	if st.Skip && st.SkipReason != "" {
		subtitle = "Skipped: " + st.SkipReason
	}
	if subtitle != "" {
		row.SetSubtitle(subtitle)
	}

	// Status indicator in the prefix slot.
	indicator := adw.NewBin()
	icon := gtk.NewImageFromIconName("content-loading-symbolic")
	icon.SetPixelSize(16)
	icon.AddCSSClass("dim-label")
	spinner := gtk.NewSpinner()
	indicator.SetChild(icon)
	row.AddPrefix(indicator)

	sr := &stepStatusRow{
		row:         row,
		indicator:   indicator,
		icon:        icon,
		spinner:     spinner,
		description: st.Description,
	}
	if st.Skip {
		sr.setStatus(core.StatusSkipped, 0, "")
	}
	return sr
}

func (sr *stepStatusRow) widget() gtk.Widgetter { return sr.row }

func (sr *stepStatusRow) setStatus(status core.StepStatus, exit int, detail string) {
	sr.spinner.Stop()
	sr.indicator.SetChild(sr.icon)
	sr.icon.RemoveCSSClass("success")
	sr.icon.RemoveCSSClass("error")
	sr.icon.RemoveCSSClass("warning")
	sr.icon.RemoveCSSClass("dim-label")
	switch status {
	case core.StatusRunning:
		sr.indicator.SetChild(sr.spinner)
		sr.spinner.Start()
		if sr.description != "" {
			sr.row.SetSubtitle(sr.description)
		} else {
			sr.row.SetSubtitle("")
		}
	case core.StatusDone:
		sr.icon.SetFromIconName("verified-checkmark-symbolic")
		sr.icon.AddCSSClass("success")
		if sr.description != "" {
			sr.row.SetSubtitle(sr.description)
		}
	case core.StatusFailed:
		sr.icon.SetFromIconName("cross-small-symbolic")
		sr.icon.AddCSSClass("error")
		if detail != "" {
			sr.row.SetSubtitle(detail)
		}
	case core.StatusSkipped:
		sr.icon.SetFromIconName("action-unavailable-symbolic")
		sr.icon.AddCSSClass("dim-label")
		// Subtitle was already set to "Skipped: …" in the constructor.
	case core.StatusCanceled:
		sr.icon.SetFromIconName("process-stop-symbolic")
		sr.icon.AddCSSClass("warning")
		sr.row.SetSubtitle("Canceled")
	default:
		sr.icon.SetFromIconName("content-loading-symbolic")
		sr.icon.AddCSSClass("dim-label")
	}
}

// writeFile is a tiny helper used by the Finish page's "Save kubeconfig"
// button.
func writeFile(path, content string) error {
	if path == "" {
		return fmt.Errorf("no path")
	}
	return os.WriteFile(path, []byte(content), 0o600)
}
