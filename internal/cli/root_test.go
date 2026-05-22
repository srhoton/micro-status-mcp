package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestNewRootCommand_HasAllSubcommands guards against regressions where a
// subcommand stops being wired up.
func TestNewRootCommand_HasAllSubcommands(t *testing.T) {
	root := NewRootCommand()
	want := map[string]bool{
		"serve":      false,
		"register":   false,
		"unregister": false,
		"post":       false,
		"list":       false,
		"sessions":   false,
	}
	for _, c := range root.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("subcommand %q not registered on root", name)
		}
	}
}

// TestRootHelp_Runs verifies the root command's --help renders without panic.
func TestRootHelp_Runs(t *testing.T) {
	root := NewRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute --help: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"serve", "register", "post", "list"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q (got: %s)", want, out)
		}
	}
}

// TestDefaultEndpointDerivesFromAddr ensures the endpoint is not a hand-typed
// duplicate of the addr constant.
func TestDefaultEndpointDerivesFromAddr(t *testing.T) {
	if !strings.HasPrefix(defaultEndpoint, "http://") {
		t.Errorf("expected defaultEndpoint to start with http://, got %q", defaultEndpoint)
	}
	if !strings.Contains(defaultEndpoint, defaultAddr) {
		t.Errorf("expected defaultEndpoint to contain defaultAddr (%q), got %q", defaultAddr, defaultEndpoint)
	}
	if !strings.HasSuffix(defaultEndpoint, mcpPath) {
		t.Errorf("expected defaultEndpoint to end with mcpPath (%q), got %q", mcpPath, defaultEndpoint)
	}
}
