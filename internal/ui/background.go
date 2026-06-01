package ui

import (
	"context"

	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// backgroundHeld is true when the application is running in background mode
// (app.Hold() has been called to prevent the app from quitting with no windows).
var backgroundHeld bool

// goBackground hides all application windows and keeps the app alive so it
// can be restored later. A desktop notification is sent to let the user know.
func goBackground(app *gtk.Application) {
	if !backgroundHeld {
		app.Hold()
		backgroundHeld = true
	}
	for _, w := range app.Windows() {
		w.Hide()
	}
	setPortalStatus("Managing Kubernetes clusters in the background")
	notif := gio.NewNotification("Orchestrator is running in the background")
	notif.SetBody("Click to reopen the window.")
	notif.SetDefaultAction("app.activate")
	app.SendNotification("background", notif)
}

// leaveBackground releases the background hold and withdraws the notification.
func leaveBackground(app *gtk.Application) {
	if backgroundHeld {
		app.Release()
		backgroundHeld = false
	}
	setPortalStatus("")
	app.WithdrawNotification("background")
}

// isLastVisibleWindow reports whether this is the only window still showing.
func isLastVisibleWindow(app *gtk.Application) bool {
	count := 0
	for _, w := range app.Windows() {
		if w.IsVisible() {
			count++
		}
	}
	return count <= 1
}

// setPortalStatus calls org.freedesktop.portal.Background.SetStatus so the
// app appears (or disappears) in the GNOME background-apps indicator.
// An empty message clears the status entry.
func setPortalStatus(message string) {
	conn, err := gio.BusGetSync(context.Background(), gio.BusTypeSession)
	if err != nil {
		return
	}

	builder := glib.NewVariantBuilder(glib.NewVariantType("a{sv}"))
	if message != "" {
		builder.Open(glib.NewVariantType("{sv}"))
		builder.AddValue(glib.NewVariantString("message"))
		builder.AddValue(glib.NewVariantVariant(glib.NewVariantString(message)))
		builder.Close()
	}
	options := builder.End()
	params := glib.NewVariantTuple([]*glib.Variant{options})

	conn.CallSync(
		context.Background(),
		"org.freedesktop.portal.Desktop",
		"/org/freedesktop/portal/desktop",
		"org.freedesktop.portal.Background",
		"SetStatus",
		params,
		nil,
		gio.DBusCallFlagsNone,
		-1,
	)
}
