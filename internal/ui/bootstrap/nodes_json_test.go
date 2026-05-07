package bootstrap

import (
	"strings"
	"testing"

	core "github.com/SilkePilon/Orchestrator/internal/bootstrap"
)

func TestNodesJSONRoundTripPreservesSensitiveFields(t *testing.T) {
	server := core.NewNode(core.RoleServer)
	server.Host = "10.0.0.1"
	server.Password = "ssh-password"
	server.Become = core.BecomeSudo
	server.BecomePassword = "sudo-password"
	server.PrivateKeyData = []byte("private-key")

	data, err := encodeNodesJSON([]core.Node{server})
	if err != nil {
		t.Fatalf("encodeNodesJSON: %v", err)
	}
	for _, want := range []string{"ssh-password", "sudo-password", "cHJpdmF0ZS1rZXk="} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("export missing %q: %s", want, data)
		}
	}

	nodes, err := decodeNodesJSON(data)
	if err != nil {
		t.Fatalf("decodeNodesJSON: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes", len(nodes))
	}
	if nodes[0].Password != server.Password || nodes[0].BecomePassword != server.BecomePassword || string(nodes[0].PrivateKeyData) != string(server.PrivateKeyData) {
		t.Fatalf("sensitive fields did not round trip: %#v", nodes[0])
	}
}

func TestDecodeNodesJSONAcceptsArrayAndDefaults(t *testing.T) {
	nodes, err := decodeNodesJSON([]byte(`[{"Role":"server","Host":"10.0.0.1"}]`))
	if err != nil {
		t.Fatalf("decodeNodesJSON: %v", err)
	}
	if nodes[0].ID == "" || nodes[0].Port != 22 || nodes[0].User != "root" || nodes[0].Auth != core.AuthAgent || nodes[0].Become != core.BecomeNone {
		t.Fatalf("defaults not applied: %#v", nodes[0])
	}
}

func TestDecodeNodesJSONRequiresOneServer(t *testing.T) {
	_, err := decodeNodesJSON([]byte(`{"version":1,"nodes":[{"Role":"agent","Host":"10.0.0.2"}]}`))
	if err == nil || !strings.Contains(err.Error(), "exactly one server") {
		t.Fatalf("expected one-server error, got %v", err)
	}
}
