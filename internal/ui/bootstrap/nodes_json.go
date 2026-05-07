package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	core "github.com/SilkePilon/Orchestrator/internal/bootstrap"
)

type nodesJSON struct {
	Version int         `json:"version"`
	Nodes   []core.Node `json:"nodes"`
}

func (w *Wizard) nodesImportButton(rebuild func(bool)) *gtk.Button {
	button := gtk.NewButtonFromIconName("document-open-symbolic")
	button.AddCSSClass("flat")
	button.SetTooltipText("Import nodes from JSON")
	button.ConnectClicked(func() {
		chooser := gtk.NewFileChooserNative("Import nodes", w.parentWindow(), gtk.FileChooserActionOpen, "Import", "Cancel")
		chooser.ConnectResponse(func(responseID int) {
			if responseID != int(gtk.ResponseAccept) || chooser.File() == nil {
				return
			}
			data, err := os.ReadFile(chooser.File().Path())
			if err != nil {
				w.errorToast(fmt.Errorf("read nodes JSON: %w", err))
				return
			}
			nodes, err := decodeNodesJSON(data)
			if err != nil {
				w.errorToast(err)
				return
			}
			d := w.draft.Value()
			d.Nodes = nodes
			w.draft.Pub(d)
			rebuild(false)
		})
		chooser.Show()
	})
	return button
}

func (w *Wizard) nodesExportButton() *gtk.Button {
	button := gtk.NewButtonFromIconName("document-save-symbolic")
	button.AddCSSClass("flat")
	button.SetTooltipText("Export nodes to JSON")
	button.ConnectClicked(func() {
		dialog := adw.NewMessageDialog(w.parentWindow(), "Export nodes to JSON?", "The exported JSON may contain sensitive text, including SSH passwords, sudo/root passwords, private key passphrases, private key data, hostnames, and usernames.")
		dialog.AddResponse("cancel", "Cancel")
		dialog.AddResponse("export", "Export")
		dialog.SetResponseAppearance("export", adw.ResponseDestructive)
		dialog.Present()
		dialog.ConnectResponse(func(response string) {
			if response != "export" {
				return
			}
			w.chooseNodesExportPath()
		})
	})
	return button
}

func (w *Wizard) chooseNodesExportPath() {
	chooser := gtk.NewFileChooserNative("Export nodes", w.parentWindow(), gtk.FileChooserActionSave, "Export", "Cancel")
	chooser.SetCurrentName("orchestrator-nodes.json")
	chooser.ConnectResponse(func(responseID int) {
		if responseID != int(gtk.ResponseAccept) || chooser.File() == nil {
			return
		}
		data, err := encodeNodesJSON(w.draft.Value().Nodes)
		if err != nil {
			w.errorToast(err)
			return
		}
		if err := os.WriteFile(chooser.File().Path(), data, 0o600); err != nil {
			w.errorToast(fmt.Errorf("write nodes JSON: %w", err))
		}
	})
	chooser.Show()
}

func encodeNodesJSON(nodes []core.Node) ([]byte, error) {
	return json.MarshalIndent(nodesJSON{Version: 1, Nodes: nodes}, "", "  ")
}

func decodeNodesJSON(data []byte) ([]core.Node, error) {
	var wrapped nodesJSON
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Nodes != nil {
		return normalizeImportedNodes(wrapped.Nodes)
	}

	var nodes []core.Node
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("parse nodes JSON: %w", err)
	}
	return normalizeImportedNodes(nodes)
}

func normalizeImportedNodes(nodes []core.Node) ([]core.Node, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("nodes JSON does not contain any nodes")
	}

	serverCount := 0
	for i := range nodes {
		if nodes[i].Role == "" {
			if i == 0 {
				nodes[i].Role = core.RoleServer
			} else {
				nodes[i].Role = core.RoleAgent
			}
		}
		if nodes[i].Role != core.RoleServer && nodes[i].Role != core.RoleAgent {
			return nil, fmt.Errorf("node %d has unknown role %q", i+1, nodes[i].Role)
		}
		if nodes[i].ID == "" {
			fresh := core.NewNode(nodes[i].Role)
			nodes[i].ID = fresh.ID
		}
		if nodes[i].Port == 0 {
			nodes[i].Port = 22
		}
		if nodes[i].User == "" {
			nodes[i].User = "root"
		}
		if nodes[i].Auth == "" {
			nodes[i].Auth = core.AuthAgent
		}
		if nodes[i].Become == "" {
			nodes[i].Become = core.BecomeNone
		}
		if nodes[i].Role == core.RoleServer {
			serverCount++
			if nodes[i].Label == "" {
				nodes[i].Label = "server-1"
			}
		} else if nodes[i].Label == "" {
			nodes[i].Label = fmt.Sprintf("agent-%d", i+1)
		}
	}
	if serverCount != 1 {
		return nil, fmt.Errorf("nodes JSON must contain exactly one server node, found %d", serverCount)
	}
	return nodes, nil
}
