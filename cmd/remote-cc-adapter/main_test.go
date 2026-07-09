package main

import (
	"path/filepath"
	"testing"

	"github.com/hoveychen/remote-cc-adapter/internal/routing"
)

// routingTableForDefaults builds the ModeLocalAllowlist table the adapter would
// construct under -default-remote, seeded only with defaultLocalPrefixes.
func routingTableForDefaults(t *testing.T) *routing.Table {
	t.Helper()
	return routing.New(routing.ModeLocalAllowlist, nil, defaultLocalPrefixes())
}

func TestDefaultLocalPrefixes(t *testing.T) {
	t.Run("HOME set, no CLAUDE_CONFIG_DIR", func(t *testing.T) {
		t.Setenv("HOME", "/home/alice")
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		got := defaultLocalPrefixes()
		want := []string{"/home/alice/.claude", "/home/alice/.claude.json"}
		assertPrefixes(t, got, want)
	})

	t.Run("CLAUDE_CONFIG_DIR overrides config home", func(t *testing.T) {
		t.Setenv("HOME", "/home/alice")
		t.Setenv("CLAUDE_CONFIG_DIR", "/etc/claude")
		got := defaultLocalPrefixes()
		// Config home comes from CLAUDE_CONFIG_DIR; ~/.claude.json still anchors to HOME.
		want := []string{"/etc/claude", "/home/alice/.claude.json"}
		assertPrefixes(t, got, want)
	})

	t.Run("no HOME, no CLAUDE_CONFIG_DIR", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		if got := defaultLocalPrefixes(); len(got) != 0 {
			t.Fatalf("want no defaults when nothing to anchor to, got %v", got)
		}
	})
}

// TestDefaultLocalPrefixes_precise guards the routing matcher's file-vs-dir
// boundary: ~/.claude must not swallow ~/.claude.json, and the exact config
// file must match itself. This is the whole point of pinning the file prefix.
func TestDefaultLocalPrefixes_matcherBoundary(t *testing.T) {
	t.Setenv("HOME", "/home/alice")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	tbl := routingTableForDefaults(t)
	cases := []struct {
		path      string
		wantLocal bool
	}{
		{"/home/alice/.claude/projects/x/session.json", true},
		{"/home/alice/.claude.json", true},
		{"/home/alice/.claude.json.bak", false},  // not the config file
		{"/home/alice/work/repo/main.go", false}, // a project file -> remote
	}
	for _, c := range cases {
		if gotRemote := tbl.IsRemote(c.path); gotRemote == c.wantLocal {
			t.Errorf("IsRemote(%q) = %v, want local=%v", c.path, gotRemote, c.wantLocal)
		}
	}
}

func assertPrefixes(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range want {
		if filepath.Clean(got[i]) != filepath.Clean(want[i]) {
			t.Errorf("prefix[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
