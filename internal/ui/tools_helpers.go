package ui

import (
	"context"
	"fmt"

	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
)

// toolPage builds the standard scaffold used by every tool page (header bar
// with title + cluster name, a dim "last updated / status" label and a
// refresh button on the right, plus a scrolled AdwPreferencesPage as the
// content). Returns the toolbar view, the inner preferences page, the refresh
// button (which the caller must wire up) and the header status label.
func toolPage(state *common.ClusterState, title, refreshTooltip string) (*adw.ToolbarView, *adw.PreferencesPage, *gtk.Button, *gtk.Label) {
	tv := adw.NewToolbarView()
	tv.AddCSSClass("view")
	tv.SetTopBarStyle(adw.ToolbarRaised)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle(title, state.ClusterPreferences.Value().Name))

	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.AddCSSClass("flat")
	refresh.SetTooltipText(refreshTooltip)
	header.PackEnd(refresh)

	status := gtk.NewLabel("")
	status.AddCSSClass("dim-label")
	status.AddCSSClass("caption")
	status.SetEllipsize(pango.EllipsizeEnd)
	status.SetMaxWidthChars(40)
	status.SetMarginEnd(6)
	header.PackEnd(status)

	tv.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	tv.SetContent(scroll)

	return tv, page, refresh, status
}

// toolGroup creates an AdwPreferencesGroup with a title and optional description.
func toolGroup(title, description string) *adw.PreferencesGroup {
	g := adw.NewPreferencesGroup()
	g.SetTitle(title)
	if description != "" {
		g.SetDescription(description)
	}
	return g
}

// statTile is a single big-number tile: large value on top, caption underneath,
// optional CSS style class ("accent" / "success" / "warning" / "error").
type statTile struct {
	Value   string
	Caption string
	Style   string
}

// statTilesGroup adds a flow box of stat tiles to a preferences group.
func statTilesGroup(group *adw.PreferencesGroup, tiles []statTile) {
	flow := gtk.NewFlowBox()
	flow.SetSelectionMode(gtk.SelectionNone)
	flow.SetColumnSpacing(12)
	flow.SetRowSpacing(12)
	flow.SetHomogeneous(true)
	flow.SetMaxChildrenPerLine(6)
	flow.SetMinChildrenPerLine(1)
	for _, t := range tiles {
		flow.Insert(buildStatTile(t), -1)
	}
	group.Add(flow)
}

func buildStatTile(t statTile) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationVertical, 4)
	box.AddCSSClass("stat-tile")
	if t.Style != "" {
		box.AddCSSClass(t.Style)
	}
	value := gtk.NewLabel(t.Value)
	value.AddCSSClass("stat-value")
	value.SetXAlign(0)
	caption := gtk.NewLabel(t.Caption)
	caption.AddCSSClass("stat-caption")
	caption.SetXAlign(0)
	caption.SetWrap(true)
	box.Append(value)
	box.Append(caption)
	return box
}

// metricRow returns an AdwActionRow with title (and optional subtitle) on the
// left and a right-aligned metric value label.
func metricRow(title, subtitle, value string) *adw.ActionRow {
	row := adw.NewActionRow()
	row.SetTitle(title)
	if subtitle != "" {
		row.SetSubtitle(subtitle)
	}
	label := gtk.NewLabel(value)
	label.AddCSSClass("metric-value")
	label.SetEllipsize(pango.EllipsizeEnd)
	label.SetMaxWidthChars(40)
	label.SetXAlign(1)
	row.AddSuffix(label)
	return row
}

// progressRow returns an AdwActionRow with a labeled progress bar suffix.
func progressRow(title, text string, value, total float64) *adw.ActionRow {
	row := adw.NewActionRow()
	row.SetTitle(title)
	bar := gtk.NewProgressBar()
	bar.SetSizeRequest(220, -1)
	bar.SetShowText(true)
	bar.SetText(text)
	bar.SetVAlign(gtk.AlignCenter)
	if total > 0 {
		f := value / total
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		bar.SetFraction(f)
	}
	row.AddSuffix(bar)
	return row
}

// findingRow returns an AdwActionRow representing a single finding with an
// icon prefix and a colored severity pill suffix.
func findingRow(title, detail, iconName, pillText, pillStyle string) *adw.ActionRow {
	row := adw.NewActionRow()
	row.SetTitle(title)
	row.SetSubtitle(detail)
	row.SetSubtitleLines(4)
	if iconName != "" {
		icon := gtk.NewImageFromIconName(iconName)
		icon.AddCSSClass(pillStyle)
		row.AddPrefix(icon)
	}
	if pillText != "" {
		pill := gtk.NewLabel(pillText)
		pill.AddCSSClass("severity-pill")
		if pillStyle != "" {
			pill.AddCSSClass(pillStyle)
		}
		pill.SetVAlign(gtk.AlignCenter)
		row.AddSuffix(pill)
	}
	return row
}

// clickableFindingRow wraps findingRow with an activation handler so the user
// can click a row to see more details. A trailing chevron makes the affordance
// obvious.
func clickableFindingRow(title, detail, iconName, pillText, pillStyle string, onClick func()) *adw.ActionRow {
	row := findingRow(title, detail, iconName, pillText, pillStyle)
	if onClick == nil {
		return row
	}
	row.SetActivatable(true)
	chevron := gtk.NewImageFromIconName("go-next-symbolic")
	chevron.AddCSSClass("dim-label")
	row.AddSuffix(chevron)
	row.ConnectActivated(onClick)
	return row
}

// detailField is one row in the details dialog.
type detailField struct {
	Label string
	Value string
}

// detailSection groups related detailFields under a heading inside the
// details dialog.
type detailSection struct {
	Title       string
	Description string
	Fields      []detailField
}

// showDetailsDialog opens an AdwDialog showing a finding's full details.
func showDetailsDialog(ctx context.Context, title string, severity int, sections []detailSection) {
	parent := ctxt.MustFrom[*gtk.Window](ctx)

	dialog := adw.NewDialog()
	dialog.SetTitle(title)
	dialog.SetContentWidth(620)
	dialog.SetContentHeight(560)

	tv := adw.NewToolbarView()
	tv.AddCSSClass("view")
	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle(title, ""))
	tv.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()

	if severity > 0 {
		style, label, icon := severityClassification(severity)
		head := adw.NewPreferencesGroup()
		row := adw.NewActionRow()
		row.SetTitle("Severity")
		row.SetSubtitle(fmt.Sprintf("%s · score %d", label, severity))
		img := gtk.NewImageFromIconName(icon)
		img.AddCSSClass(style)
		row.AddPrefix(img)
		pill := gtk.NewLabel(label)
		pill.AddCSSClass("severity-pill")
		pill.AddCSSClass(style)
		pill.SetVAlign(gtk.AlignCenter)
		row.AddSuffix(pill)
		head.Add(row)
		page.Add(head)
	}

	for _, section := range sections {
		if len(section.Fields) == 0 && section.Description == "" {
			continue
		}
		group := adw.NewPreferencesGroup()
		if section.Title != "" {
			group.SetTitle(section.Title)
		}
		if section.Description != "" {
			group.SetDescription(section.Description)
		}
		for _, f := range section.Fields {
			value := f.Value
			if value == "" {
				value = "—"
			}
			row := adw.NewActionRow()
			row.SetTitle(f.Label)
			row.SetSubtitle(value)
			row.SetSubtitleLines(0)
			row.SetSubtitleSelectable(true)
			group.Add(row)
		}
		page.Add(group)
	}

	scroll.SetChild(page)
	tv.SetContent(scroll)
	dialog.SetChild(tv)
	dialog.Present(parent)
}

// when there is nothing to list. Uses a compact AdwActionRow so it sits well
// inside a boxed-list page.
func emptyStatusGroup(group *adw.PreferencesGroup, title, detail, iconName string) {
	row := adw.NewActionRow()
	row.SetTitle(title)
	row.SetSubtitle(detail)
	if iconName != "" {
		icon := gtk.NewImageFromIconName(iconName)
		icon.AddCSSClass("success")
		row.AddPrefix(icon)
	}
	group.Add(row)
}

// statusLabel builds the dim "last updated" label commonly shown in tool
// page headers.
func statusLabel(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.SetHAlign(gtk.AlignStart)
	l.AddCSSClass("dim-label")
	return l
}

// toolStatusGroup is a small group used to surface the "loading / updated at"
// message at the very top of a tool page.
func toolStatusGroup(label *gtk.Label) *adw.PreferencesGroup {
	g := adw.NewPreferencesGroup()
	g.Add(label)
	return g
}

// severityClassification returns the standard pill style, label, and
// icon-name for a numeric severity.
func severityClassification(severity int) (style, label, icon string) {
	switch {
	case severity >= 90:
		return "error", "Critical", "dialog-error-symbolic"
	case severity >= 70:
		return "error", "High", "dialog-warning-symbolic"
	case severity >= 40:
		return "warning", "Medium", "dialog-warning-symbolic"
	default:
		return "accent", "Low", "dialog-information-symbolic"
	}
}

// healthBadge returns the style + text for a healthy/unhealthy ratio (e.g.
// "5 / 5" → success; "2 / 5" → warning).
func healthBadge(ready, total int) (style, text string) {
	if total <= 0 {
		return "accent", "—"
	}
	text = fmt.Sprintf("%d / %d", ready, total)
	switch {
	case ready >= total:
		return "success", text
	case ready*2 < total:
		return "error", text
	default:
		return "warning", text
	}
}

// removeChild removes the given widget from its parent, if any. Used during
// page refreshes to drop the previous content groups.
func removeChild(w gtk.Widgetter) {
	if w == nil {
		return
	}
	base := gtk.BaseWidget(w)
	parent := base.Parent()
	if parent == nil {
		return
	}
	if box, ok := parent.(*gtk.Box); ok {
		box.Remove(w)
		return
	}
	if page, ok := parent.(*adw.PreferencesPage); ok {
		if group, ok := w.(*adw.PreferencesGroup); ok {
			page.Remove(group)
			return
		}
	}
	base.Unparent()
}

// _ keeps context import used by future helpers (avoids unused-import churn
// when individual pages stop importing it directly).
var _ = context.Background
