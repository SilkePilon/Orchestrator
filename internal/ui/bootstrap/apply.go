package bootstrap

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	core "github.com/SilkePilon/Orchestrator/internal/bootstrap"
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

	body := gtk.NewBox(gtk.OrientationVertical, 0)

	tabView := adw.NewTabView()
	tabBar := adw.NewTabBar()
	tabBar.SetView(tabView)
	body.Append(tabBar)

	tabView.SetVExpand(true)
	body.Append(tabView)

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

	bottom := gtk.NewBox(gtk.OrientationHorizontal, 12)
	bottom.SetMarginTop(8)
	bottom.SetMarginBottom(8)
	bottom.SetMarginStart(12)
	bottom.SetMarginEnd(12)
	progress := gtk.NewProgressBar()
	progress.SetHExpand(true)
	progress.SetShowText(true)
	progress.SetText("Starting…")
	bottom.Append(progress)

	ctx, cancel := context.WithCancel(w.ctx)
	aborted := &atomic.Bool{}

	abort := gtk.NewButtonWithLabel("Abort")
	abort.AddCSSClass("destructive-action")
	abort.ConnectClicked(func() {
		aborted.Store(true)
		progress.SetText("Aborting…")
		abort.SetSensitive(false)
		cancel()
	})
	bottom.Append(abort)

	body.Append(bottom)

	page := w.pageShell("Apply", "", body, nil)

	// Drive the executor on a goroutine; marshal events to the UI.
	go w.runApply(ctx, cancel, aborted, logs, tabView, tabPages, progress, abort)

	return page
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
				progress.SetText(fmt.Sprintf("%d / %d steps", doneSteps, totalSteps))
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
			progress.SetText("Done")
		} else if aborted.Load() {
			progress.SetText("Canceled")
		} else {
			progress.SetText("Failed")
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

	listBox := gtk.NewListBox()
	listBox.AddCSSClass("navigation-sidebar")
	listScroll := gtk.NewScrolledWindow()
	listScroll.SetChild(listBox)
	listScroll.SetSizeRequest(260, -1)
	pane.SetStartChild(listScroll)
	pane.SetResizeStartChild(false)

	view := gtk.NewTextView()
	view.SetMonospace(true)
	view.SetEditable(false)
	view.SetCursorVisible(false)
	view.SetWrapMode(gtk.WrapWordChar)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetHExpand(true)
	scroll.SetChild(view)
	pane.SetEndChild(scroll)
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
			r.setStatus(core.StatusRunning, 0)
		}
	case "step.end":
		if r, ok := nl.steps[ev.StepID]; ok {
			r.setStatus(ev.Status, ev.ExitCode)
		}
		if ev.Status == core.StatusFailed {
			nl.failed = true
			nl.appendLine(fmt.Sprintf("✗ step failed (exit %d): %v", ev.ExitCode, ev.Err))
		}
	case "stdout", "stderr", "log":
		nl.appendLine(ev.Line)
	}
}

func (nl *nodeLog) appendLine(line string) {
	end := nl.buf.EndIter()
	stamp := time.Now().Format("15:04:05")
	nl.buf.Insert(end, fmt.Sprintf("[%s] %s\n", stamp, line))
	// Autoscroll: scroll the view to the end mark.
	mark := nl.buf.CreateMark("end", nl.buf.EndIter(), false)
	nl.view.ScrollMarkOnscreen(mark)
	nl.buf.DeleteMark(mark)
}

// stepStatusRow is the sidebar entry for a single step.
type stepStatusRow struct {
	row       *gtk.Box
	indicator *adw.Bin
	icon      *gtk.Image
	spinner   *gtk.Spinner
	title     *gtk.Label
}

func newStepStatusRow(st core.Step) *stepStatusRow {
	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	row.SetMarginTop(4)
	row.SetMarginBottom(4)
	row.SetMarginStart(8)
	row.SetMarginEnd(8)
	indicator := adw.NewBin()
	indicator.SetSizeRequest(12, 12)
	icon := gtk.NewImageFromIconName("content-loading-symbolic")
	icon.SetPixelSize(12)
	icon.AddCSSClass("dim-label")
	spinner := gtk.NewSpinner()
	spinner.SetSizeRequest(12, 12)
	indicator.SetChild(icon)
	row.Append(indicator)
	title := gtk.NewLabel(st.Title)
	title.SetXAlign(0)
	title.SetEllipsize(2) // PANGO_ELLIPSIZE_END
	row.Append(title)
	statusRow := &stepStatusRow{row: row, indicator: indicator, icon: icon, spinner: spinner, title: title}
	if st.Skip {
		statusRow.setStatus(core.StatusSkipped, 0)
	}
	return statusRow
}

func (statusRow *stepStatusRow) widget() gtk.Widgetter { return statusRow.row }

func (statusRow *stepStatusRow) setStatus(status core.StepStatus, exit int) {
	statusRow.spinner.Stop()
	statusRow.indicator.SetChild(statusRow.icon)
	statusRow.icon.RemoveCSSClass("success")
	statusRow.icon.RemoveCSSClass("error")
	statusRow.icon.RemoveCSSClass("warning")
	statusRow.icon.RemoveCSSClass("dim-label")
	switch status {
	case core.StatusRunning:
		statusRow.indicator.SetChild(statusRow.spinner)
		statusRow.spinner.Start()
	case core.StatusDone:
		statusRow.icon.SetFromIconName("verified-checkmark-symbolic")
		statusRow.icon.AddCSSClass("success")
	case core.StatusFailed:
		statusRow.icon.SetFromIconName("cross-small-symbolic")
		statusRow.icon.AddCSSClass("error")
		statusRow.title.SetText(statusRow.title.Text() + fmt.Sprintf(" (exit %d)", exit))
	case core.StatusSkipped:
		statusRow.icon.SetFromIconName("action-unavailable-symbolic")
		statusRow.icon.AddCSSClass("dim-label")
	case core.StatusCanceled:
		statusRow.icon.SetFromIconName("process-stop-symbolic")
		statusRow.icon.AddCSSClass("warning")
	default:
		statusRow.icon.SetFromIconName("content-loading-symbolic")
		statusRow.icon.AddCSSClass("dim-label")
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
