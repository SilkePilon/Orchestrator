package ui

import (
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

var Version string = "dev"

type AboutWindow struct {
	*adw.AboutDialog
}

func NewAboutWindow() *AboutWindow {
	w := AboutWindow{adw.NewAboutDialog()}
	w.SetApplicationIcon("orchestrator")
	w.SetApplicationName(ApplicationName)
	w.SetVersion(Version)
	w.SetWebsite("https://github.com/SilkePilon/Orchestrator")
	w.SetIssueURL("https://github.com/SilkePilon/Orchestrator/issues")
	w.SetSupportURL("https://github.com/SilkePilon/Orchestrator/discussions")
	w.SetLicenseType(gtk.LicenseMPL20)
	return &w
}
