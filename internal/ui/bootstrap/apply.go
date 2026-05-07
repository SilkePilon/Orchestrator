package bootstrap

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	core "github.com/getseabird/seabird/internal/bootstrap"
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

	abort := gtk.NewButtonWithLabel("Abort")
	abort.AddCSSClass("destructive-action")
	abort.ConnectClicked(func() { cancel() })
	bottom.Append(abort)

	body.Append(bottom)

	page := w.pageShell("Apply", "", body, nil)

	// Drive the executor on a goroutine; marshal events to the UI.
	go w.runApply(ctx, cancel, logs, progress, abort)

	return page
}

func (w *Wizard) runApply(
	ctx context.Context,
	cancel context.CancelFunc,
	logs map[string]*nodeLog,
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
	cancel()

	// Did any step fail?
	finalErr := runErr
	for _, nl := range logs {
		if nl.failed {
			if finalErr == nil {
				finalErr = fmt.Errorf("one or more steps failed")
			}
		}
	}

	kubeconfig := exec.KubeconfigYAML()
	success := finalErr == nil && kubeconfig != ""

	glib.IdleAdd(func() {
		abort.SetSensitive(false)
		if success {
			progress.SetFraction(1)
			progress.SetText("Done")
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
	row   *gtk.Box
	icon  *gtk.Image
	title *gtk.Label
}

func newStepStatusRow(st core.Step) *stepStatusRow {
	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	row.SetMarginTop(4)
	row.SetMarginBottom(4)
	row.SetMarginStart(8)
	row.SetMarginEnd(8)
	icon := gtk.NewImageFromIconName("content-loading-symbolic")
	icon.AddCSSClass("dim-label")
	row.Append(icon)
	title := gtk.NewLabel(st.Title)
	title.SetXAlign(0)
	title.SetEllipsize(2) // PANGO_ELLIPSIZE_END
	row.Append(title)
	r := &stepStatusRow{row: row, icon: icon, title: title}
	if st.Skip {
		r.setStatus(core.StatusSkipped, 0)
	}
	return r
}

func (r *stepStatusRow) widget() gtk.Widgetter { return r.row }

func (r *stepStatusRow) setStatus(status core.StepStatus, exit int) {
	r.icon.RemoveCSSClass("success")
	r.icon.RemoveCSSClass("error")
	r.icon.RemoveCSSClass("warning")
	r.icon.RemoveCSSClass("dim-label")
	switch status {
	case core.StatusRunning:
		r.icon.SetFromIconName("content-loading-symbolic")
	case core.StatusDone:
		r.icon.SetFromIconName("emblem-ok-symbolic")
		r.icon.AddCSSClass("success")
	case core.StatusFailed:
		r.icon.SetFromIconName("dialog-error-symbolic")
		r.icon.AddCSSClass("error")
		r.title.SetText(r.title.Text() + fmt.Sprintf(" (exit %d)", exit))
	case core.StatusSkipped:
		r.icon.SetFromIconName("action-unavailable-symbolic")
		r.icon.AddCSSClass("dim-label")
	case core.StatusCanceled:
		r.icon.SetFromIconName("process-stop-symbolic")
		r.icon.AddCSSClass("warning")
	default:
		r.icon.SetFromIconName("content-loading-symbolic")
		r.icon.AddCSSClass("dim-label")
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
