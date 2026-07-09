package main

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/hoveychen/remote-cc-adapter/internal/routing"
)

func TestParseRunArgs(t *testing.T) {
	t.Run("flags after the command (boss UX)", func(t *testing.T) {
		o, err := parseRunArgs([]string{"claude", "--code", "rca1.xyz"})
		if err != nil {
			t.Fatal(err)
		}
		if o.command != "claude" || o.code != "rca1.xyz" || len(o.args) != 0 {
			t.Fatalf("got command=%q code=%q args=%v", o.command, o.code, o.args)
		}
	})

	t.Run("flags before the command", func(t *testing.T) {
		o, err := parseRunArgs([]string{"--code", "rca1.xyz", "claude", "-p", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if o.command != "claude" || !reflect.DeepEqual(o.args, []string{"-p", "hi"}) {
			t.Fatalf("got command=%q args=%v", o.command, o.args)
		}
	})

	t.Run("owned flags interleaved with command args", func(t *testing.T) {
		o, err := parseRunArgs([]string{"claude", "-p", "hi", "--code", "rca1.xyz", "--workdir", "/w"})
		if err != nil {
			t.Fatal(err)
		}
		if o.workdir != "/w" || !reflect.DeepEqual(o.args, []string{"-p", "hi"}) {
			t.Fatalf("got workdir=%q args=%v", o.workdir, o.args)
		}
	})

	t.Run("double-dash passes everything to the command", func(t *testing.T) {
		o, err := parseRunArgs([]string{"--code", "rca1.xyz", "--", "claude", "--code", "not-ours"})
		if err != nil {
			t.Fatal(err)
		}
		if o.command != "claude" || !reflect.DeepEqual(o.args, []string{"--code", "not-ours"}) {
			t.Fatalf("got command=%q args=%v", o.command, o.args)
		}
		if o.code != "rca1.xyz" {
			t.Fatalf("code = %q", o.code)
		}
	})

	t.Run("equals form and repeatable prefixes", func(t *testing.T) {
		o, err := parseRunArgs([]string{"--code=rca1.xyz", "--remote-prefix=/a", "--remote-prefix", "/b", "sh"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(o.remotePrefixes, []string{"/a", "/b"}) {
			t.Fatalf("remotePrefixes = %v", o.remotePrefixes)
		}
	})

	t.Run("bool flag with explicit value", func(t *testing.T) {
		o, err := parseRunArgs([]string{"--sock", "/tmp/x.sock", "--resign=false", "claude"})
		if err != nil {
			t.Fatal(err)
		}
		if o.resign {
			t.Fatal("resign should be false")
		}
	})

	t.Run("resign defaults per platform", func(t *testing.T) {
		o, err := parseRunArgs([]string{"--sock", "/tmp/x.sock", "claude"})
		if err != nil {
			t.Fatal(err)
		}
		if want := runtime.GOOS == "darwin"; o.resign != want {
			t.Fatalf("resign default = %v, want %v", o.resign, want)
		}
	})

	t.Run("unknown flag before command is an error", func(t *testing.T) {
		if _, err := parseRunArgs([]string{"--cod", "x", "claude"}); err == nil {
			t.Fatal("want error for unknown flag before command")
		}
	})

	t.Run("unknown flag after command belongs to it", func(t *testing.T) {
		o, err := parseRunArgs([]string{"claude", "--model", "opus", "--code", "rca1.xyz"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(o.args, []string{"--model", "opus"}) {
			t.Fatalf("args = %v", o.args)
		}
	})

	t.Run("no command is an error", func(t *testing.T) {
		if _, err := parseRunArgs([]string{"--code", "rca1.xyz"}); err == nil {
			t.Fatal("want error when no command given")
		}
	})

	t.Run("serve-fs-only needs no command", func(t *testing.T) {
		if _, err := parseRunArgs([]string{"--serve-fs-only", "--sock", "/tmp/x.sock"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("exactly one transport", func(t *testing.T) {
		if _, err := parseRunArgs([]string{"claude"}); err == nil {
			t.Fatal("want error with no transport flag")
		}
		if _, err := parseRunArgs([]string{"claude", "--code", "x", "--sock", "/s"}); err == nil {
			t.Fatal("want error with two transport flags")
		}
	})

	t.Run("value flag missing its value", func(t *testing.T) {
		if _, err := parseRunArgs([]string{"claude", "--code"}); err == nil {
			t.Fatal("want error for --code without value")
		}
	})
}

// routingTableForDefaults builds the ModeLocalAllowlist table run mode would
// construct under --default-remote, seeded only with defaultLocalPrefixes.
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

// TestDefaultLocalPrefixes_matcherBoundary guards the routing matcher's
// file-vs-dir boundary: ~/.claude must not swallow ~/.claude.json, and the
// exact config file must match itself.
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
