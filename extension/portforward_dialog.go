package extension

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"

	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"k8s.io/apimachinery/pkg/types"
)

// showPortForwardDialog presents an Adwaita dialog matching the Argo CD
// credentials dialog style.  It lets the user choose a local port (or randomise
// it with the dice button), open the forwarded URL in their browser, or copy
// the URL to the clipboard.
//
// The port-forward itself is started with the cluster-level context so it
// survives sheet / dialog close events and only stops when the cluster
// disconnects or the user explicitly stops it via showStopForwardDialog.
func (p *PortForwarder) showPortForwardDialog(
	widgetCtx context.Context,
	parent gtk.Widgetter,
	name types.NamespacedName,
	ports []string,
	onClose func(),
) {
	containerPort := parseContainerPort(ports[0])
	initialLocal := containerPort

	dialog := adw.NewDialog()
	dialog.SetTitle("Port Forward")
	dialog.SetContentWidth(400)
	dialog.ConnectClosed(onClose)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	header := adw.NewHeaderBar()
	box.Append(header)

	page := adw.NewPreferencesPage()
	box.Append(page)

	group := adw.NewPreferencesGroup()
	group.SetTitle("Local Port")
	group.SetDescription(fmt.Sprintf("Forward container port %d to your localhost.", containerPort))
	page.Add(group)

	portRow := adw.NewEntryRow()
	portRow.SetTitle("Local Port")
	portRow.SetText(strconv.Itoa(int(initialLocal)))
	portRow.SetInputPurpose(gtk.InputPurposeNumber)
	group.Add(portRow)

	diceBtn := gtk.NewButtonFromIconName("media-random-symbolic")
	diceBtn.SetTooltipText("Pick a random port")
	diceBtn.AddCSSClass("flat")
	diceBtn.SetVAlign(gtk.AlignCenter)
	diceBtn.ConnectClicked(func() {
		port := 10000 + rand.Intn(55535)
		portRow.SetText(strconv.Itoa(port))
	})
	portRow.AddSuffix(diceBtn)

	localPortFromEntry := func() int32 {
		text := strings.TrimSpace(portRow.Text())
		if v, err := strconv.Atoi(text); err == nil && v > 0 && v <= 65535 {
			return int32(v)
		}
		return initialLocal
	}

	footerBox := gtk.NewBox(gtk.OrientationHorizontal, 6)
	footerBox.SetMarginStart(12)
	footerBox.SetMarginEnd(12)
	footerBox.SetMarginTop(6)
	footerBox.SetMarginBottom(12)

	openBtn := gtk.NewButtonWithLabel("Open in Browser")
	openBtn.AddCSSClass("suggested-action")
	openBtn.SetHExpand(true)

	copyBtn := gtk.NewButtonFromIconName("edit-copy-symbolic")
	copyBtn.SetTooltipText("Copy URL")
	copyBtn.SetSizeRequest(36, -1)

	openBtn.ConnectClicked(func() {
		localPort := localPortFromEntry()
		portSpec := fmt.Sprintf("%d:%d", localPort, containerPort)

		_ = p.Close(name)
		dialog.Close() // triggers onClose → button refreshes to inactive while forward starts

		// Use the cluster-level context so forwarding is not tied to this widget.
		go func() {
			if err := p.New(p.clusterCtx, name, []string{portSpec}); err != nil {
				glib.IdleAdd(func() {
					widget.ShowErrorDialog(widgetCtx, "Port forward error", err)
					onClose() // refresh button to reflect no active forward
				})
				return
			}
			url := fmt.Sprintf("http://localhost:%d", localPort)
			glib.IdleAdd(func() {
				// Forward is now up — refresh the button to show the active state.
				onClose()
				var window *gtk.Window
				if w, ok := ctxt.From[*gtk.Window](widgetCtx); ok {
					window = w
				}
				gtk.ShowURI(window, url, 0)
			})
		}()
	})

	copyBtn.ConnectClicked(func() {
		localPort := localPortFromEntry()
		url := fmt.Sprintf("http://localhost:%d", localPort)
		if d := gdk.DisplayGetDefault(); d != nil {
			d.Clipboard().SetText(url)
		}
	})

	footerBox.Append(openBtn)
	footerBox.Append(copyBtn)
	box.Append(footerBox)

	dialog.SetChild(box)
	dialog.Present(parent)
}

// showStopForwardDialog shows a confirmation dialog before stopping an active
// port-forward session.  Confirming closes the forward and refreshes the button;
// cancelling leaves everything unchanged.
func (p *PortForwarder) showStopForwardDialog(
	ctx context.Context,
	parent gtk.Widgetter,
	name types.NamespacedName,
	localPort, remotePort uint16,
	ports []string,
) {
	var window *gtk.Window
	if w, ok := ctxt.From[*gtk.Window](ctx); ok {
		window = w
	}

	dlg := adw.NewAlertDialog(
		"Stop Port Forwarding?",
		fmt.Sprintf(
			"localhost:%d → %s (port %d) will no longer be accessible.",
			localPort, name.Name, remotePort,
		),
	)
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("stop", "Stop Forwarding")
	dlg.SetResponseAppearance("stop", adw.ResponseDestructive)
	dlg.SetDefaultResponse("cancel")
	dlg.ConnectResponse(func(response string) {
		if response == "stop" {
			_ = p.Close(name)
			// Refresh the button: climb up to the nearest Button ancestor of parent.
			if btn, ok := parent.(*gtk.Button); ok {
				p.UpdateButton(ctx, btn, name, ports)
			}
		}
	})
	dlg.Present(window)
}

// parseContainerPort parses the container port from a port spec of the form
// ":8080" as used throughout the port-forwarding helpers.
func parseContainerPort(spec string) int32 {
	s := strings.TrimPrefix(spec, ":")
	v, _ := strconv.Atoi(s)
	return int32(v)
}

