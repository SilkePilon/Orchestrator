package widget

import (
	"context"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
)

func ShowErrorDialog(ctx context.Context, title string, err error) *adw.AlertDialog {
	parent := ctxt.MustFrom[*gtk.Window](ctx)
	dialog := adw.NewAlertDialog(title, err.Error())
	dialog.AddResponse("ok", "Ok")
	dialog.Present(parent)
	return dialog
}
