package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stephenrhoton/micro-status-mcp/internal/server"
)

func TestResolveBody_Inline(t *testing.T) {
	got, err := resolveBody("hello", "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("resolveBody: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestResolveBody_Empty(t *testing.T) {
	got, err := resolveBody("", "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("resolveBody: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty body, got %q", got)
	}
}

func TestResolveBody_BothSetIsError(t *testing.T) {
	if _, err := resolveBody("inline", "file", strings.NewReader("")); err == nil {
		t.Error("expected error when both --body and --body-file set")
	}
}

func TestResolveBody_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "msg.txt")
	if err := os.WriteFile(path, []byte("from file\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	got, err := resolveBody("", path, strings.NewReader(""))
	if err != nil {
		t.Fatalf("resolveBody: %v", err)
	}
	if got != "from file\n" {
		t.Errorf("got %q, want %q", got, "from file\n")
	}
}

func TestResolveBody_FromStdin(t *testing.T) {
	got, err := resolveBody("", "-", strings.NewReader("piped body"))
	if err != nil {
		t.Fatalf("resolveBody: %v", err)
	}
	if got != "piped body" {
		t.Errorf("got %q, want %q", got, "piped body")
	}
}

func TestResolveBody_InlineTooLarge(t *testing.T) {
	big := strings.Repeat("a", server.MaxBodyBytes+1)
	_, err := resolveBody(big, "", strings.NewReader(""))
	if err == nil {
		t.Error("expected error for oversized inline body")
	}
}

func TestResolveBody_StdinTooLarge(t *testing.T) {
	big := strings.Repeat("a", server.MaxBodyBytes+1)
	_, err := resolveBody("", "-", strings.NewReader(big))
	if err == nil {
		t.Error("expected error for oversized stdin body")
	}
}

func TestResolveBody_FileMissing(t *testing.T) {
	_, err := resolveBody("", filepath.Join(t.TempDir(), "nope"), strings.NewReader(""))
	if err == nil {
		t.Error("expected error for missing body file")
	}
}

func TestReadCapped_AcceptsExactMax(t *testing.T) {
	exact := strings.Repeat("a", server.MaxBodyBytes)
	got, err := readCapped(bytes.NewReader([]byte(exact)), "test")
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if len(got) != server.MaxBodyBytes {
		t.Errorf("expected %d bytes, got %d", server.MaxBodyBytes, len(got))
	}
}
