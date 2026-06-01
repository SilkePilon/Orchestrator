package extension

import (
	"context"
	"strings"

	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// showEditDialog pops an AlertDialog for editing a text value.
// Long or multi-line values get a scrolled text view; short values get an Entry.
func showEditDialog(ctx context.Context, title, currentValue string, onSave func(string)) {
	dialog := adw.NewAlertDialog(title, "")
	multiline := strings.ContainsRune(currentValue, '\n') || len(currentValue) > 80

	var getValue func() string
	if multiline {
		tv := gtk.NewTextView()
		tv.SetMonospace(true)
		tv.SetWrapMode(gtk.WrapWordChar)
		tv.SetLeftMargin(8)
		tv.SetTopMargin(8)
		tv.SetRightMargin(8)
		tv.SetBottomMargin(8)
		tv.Buffer().SetText(currentValue)
		getValue = func() string {
			buf := tv.Buffer()
			return buf.Text(buf.StartIter(), buf.EndIter(), false)
		}
		scroll := gtk.NewScrolledWindow()
		scroll.SetMinContentHeight(150)
		scroll.SetMinContentWidth(380)
		scroll.SetVExpand(true)
		scroll.SetChild(tv)
		scroll.AddCSSClass("card")
		dialog.SetExtraChild(scroll)
	} else {
		entry := gtk.NewEntry()
		entry.SetText(currentValue)
		entry.SetWidthChars(42)
		getValue = func() string { return entry.Text() }
		dialog.SetExtraChild(entry)
	}

	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("save", "Save")
	dialog.SetDefaultResponse("save")
	dialog.SetResponseAppearance("save", adw.ResponseSuggested)
	dialog.ConnectResponse(func(response string) {
		if response == "save" {
			onSave(getValue())
		}
	})
	dialog.Present(ctxt.MustFrom[*gtk.Window](ctx))
}

// showAddPairDialog pops an AlertDialog for entering a new key-value pair.
func showAddPairDialog(ctx context.Context, title, keyPlaceholder, valuePlaceholder string, onAdd func(key, value string)) {
	dialog := adw.NewAlertDialog(title, "")

	keyEntry := gtk.NewEntry()
	keyEntry.SetPlaceholderText(keyPlaceholder)

	valueEntry := gtk.NewEntry()
	valueEntry.SetPlaceholderText(valuePlaceholder)

	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.SetMarginTop(4)
	box.Append(keyEntry)
	box.Append(valueEntry)
	dialog.SetExtraChild(box)

	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("add", "Add")
	dialog.SetDefaultResponse("add")
	dialog.SetResponseAppearance("add", adw.ResponseSuggested)
	dialog.ConnectResponse(func(response string) {
		if response == "add" && keyEntry.Text() != "" {
			onAdd(keyEntry.Text(), valueEntry.Text())
		}
	})
	dialog.Present(ctxt.MustFrom[*gtk.Window](ctx))
}
