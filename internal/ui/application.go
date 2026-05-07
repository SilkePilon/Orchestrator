package ui

import (
	"context"
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/icon"
	"github.com/SilkePilon/Orchestrator/internal/style"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"k8s.io/klog/v2"
)

const ApplicationName = "Orchestrator"

type Application struct {
	*adw.Application
	version string
}

func NewApplication(version string) (*Application, error) {
	gtk.Init()

	switch style.Get() {
	case style.Darwin:
		gtk.SettingsGetDefault().SetObjectProperty("gtk-decoration-layout", "close,minimize,maximize")
	}

	if err := icon.Register(); err != nil {
		klog.Infof("failed to load icons: %v", err)
	}

	ctx := context.Background()

	state, err := common.NewState()
	if err != nil {
		return nil, err
	}

	state.Preferences.Sub(ctx, func(p api.Preferences) {
		adw.StyleManagerGetDefault().SetColorScheme(adw.ColorScheme(p.ColorScheme))
	})

	style.Load()

	flags := gio.ApplicationFlagsNone
	if version == "dev" || os.Getenv("ORCHESTRATOR_DEV") == "1" {
		flags = gio.ApplicationNonUnique
	}

	a := Application{
		Application: adw.NewApplication("dev.silkepilon.Orchestrator", flags),
		version:     version,
	}

	a.ConnectActivate(func() {
		NewWelcomeWindow(ctx, &a.Application.Application, state).Present()
	})

	return &a, nil
}

func (a *Application) Run() {
	// TODO doesn't work
	// debug.SetPanicOnFault(true)
	// defer func() {
	// 	if err := recover(); err != nil {
	// 		NewPanicWindow(err).Present()
	// 	}
	// }()

	if code := a.Application.Run(os.Args); code > 0 {
		os.Exit(code)
	}
}
