package main

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/hoveychen/remote-adapter/internal/routing"
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
// construct under --default-remote for the claude profile.
func routingTableForDefaults(t *testing.T) *routing.Table {
	t.Helper()
	return routing.New(routing.ModeLocalAllowlist, nil, profileLocalPrefixes("claude"))
}

// claudeManaged / codexManaged are the absolute system-config paths each profile
// always pins local (managed/enterprise config), independent of HOME.
var claudeManaged = []string{
	"/etc/claude-code/managed-settings.json",
	"/Library/Application Support/ClaudeCode/managed-settings.json",
}
var codexManaged = []string{"/etc/codex"}

func TestProfileLocalPrefixes_claude(t *testing.T) {
	t.Run("HOME set, no CLAUDE_CONFIG_DIR", func(t *testing.T) {
		t.Setenv("HOME", "/home/alice")
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		got := profileLocalPrefixes("claude")
		want := append([]string{"/home/alice/.claude", "/home/alice/.claude.json"}, claudeManaged...)
		assertPrefixes(t, got, want)
	})

	t.Run("CLAUDE_CONFIG_DIR overrides config home", func(t *testing.T) {
		t.Setenv("HOME", "/home/alice")
		t.Setenv("CLAUDE_CONFIG_DIR", "/etc/claude")
		got := profileLocalPrefixes("claude")
		// Config home comes from CLAUDE_CONFIG_DIR; ~/.claude.json still anchors to HOME.
		want := append([]string{"/etc/claude", "/home/alice/.claude.json"}, claudeManaged...)
		assertPrefixes(t, got, want)
	})

	t.Run("no HOME, no CLAUDE_CONFIG_DIR", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		// HOME-relative prefixes drop out, but managed system paths still pin.
		got := profileLocalPrefixes("claude")
		assertPrefixes(t, got, claudeManaged)
	})
}

func TestProfileLocalPrefixes_codex(t *testing.T) {
	t.Run("HOME set, no CODEX_HOME", func(t *testing.T) {
		t.Setenv("HOME", "/home/alice")
		t.Setenv("CODEX_HOME", "")
		got := profileLocalPrefixes("codex")
		// All user codex state lives under ~/.codex (no separate top-level dot-file);
		// /etc/codex is the system config folder.
		want := append([]string{"/home/alice/.codex"}, codexManaged...)
		assertPrefixes(t, got, want)
	})

	t.Run("CODEX_HOME replaces config home", func(t *testing.T) {
		t.Setenv("HOME", "/home/alice")
		t.Setenv("CODEX_HOME", "/srv/codex")
		got := profileLocalPrefixes("codex")
		want := append([]string{"/srv/codex"}, codexManaged...)
		assertPrefixes(t, got, want)
	})

	t.Run("no HOME, no CODEX_HOME", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("CODEX_HOME", "")
		// HOME-relative config home drops out, but /etc/codex still pins.
		got := profileLocalPrefixes("codex")
		assertPrefixes(t, got, codexManaged)
	})
}

func TestProfileLocalPrefixes_unknown(t *testing.T) {
	t.Setenv("HOME", "/home/alice")
	if got := profileLocalPrefixes("hermes"); len(got) != 0 {
		t.Fatalf("unknown profile should yield no prefixes, got %v", got)
	}
	if got := profileLocalPrefixes(""); len(got) != 0 {
		t.Fatalf("empty profile should yield no prefixes, got %v", got)
	}
}

func TestDetectProfile(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"claude", "claude"},
		{"codex", "codex"},
		{"/usr/local/bin/codex", "codex"},
		{"/opt/anthropic/claude", "claude"},
		{"node", ""},
		{"", ""},
		{"codex-dev", ""}, // exact basename match only
	}
	for _, c := range cases {
		if got := detectProfile(c.command); got != c.want {
			t.Errorf("detectProfile(%q) = %q, want %q", c.command, got, c.want)
		}
	}
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

// TestOSHintArgs reproduces the cross-OS command-mismatch bug: run mode launches
// the agent on the local host (so its environment block reports the local OS),
// but routed subprocesses execute on the executor. When the two differ, the
// agent must be told the real execution platform, or it emits wrong-flavour
// commands (BSD sed/ls on a GNU/Linux executor) that fail repeatedly.
func TestOSHintArgs(t *testing.T) {
	t.Run("cross-OS claude gets an informative hint", func(t *testing.T) {
		got := osHintArgs("claude", "darwin", "linux", "arm64")
		if len(got) != 2 {
			t.Fatalf("want [flag, text], got %v", got)
		}
		if got[0] != "--append-system-prompt" {
			t.Errorf("flag = %q, want --append-system-prompt", got[0])
		}
		// The hint must actually name the executor's platform so the model knows
		// which command dialect to use — an empty/placeholder string is the bug.
		if !strings.Contains(got[1], "linux") {
			t.Errorf("hint text %q does not mention the executor OS %q", got[1], "linux")
		}
		if !strings.Contains(got[1], "arm64") {
			t.Errorf("hint text %q does not mention the executor arch %q", got[1], "arm64")
		}
	})

	t.Run("same OS injects nothing", func(t *testing.T) {
		if got := osHintArgs("claude", "linux", "linux", "amd64"); got != nil {
			t.Errorf("same-OS deployment should inject no hint, got %v", got)
		}
		if got := osHintArgs("claude", "darwin", "darwin", "arm64"); got != nil {
			t.Errorf("same-OS deployment should inject no hint, got %v", got)
		}
	})

	t.Run("unknown executor OS injects nothing", func(t *testing.T) {
		if got := osHintArgs("claude", "darwin", "", ""); got != nil {
			t.Errorf("unknown executor OS should inject no hint, got %v", got)
		}
	})

	t.Run("engine without a hint flag injects nothing", func(t *testing.T) {
		if got := osHintArgs("hermes", "darwin", "linux", "arm64"); got != nil {
			t.Errorf("unknown engine should inject no hint, got %v", got)
		}
		// codex has no --append-system-prompt flag (verified 2026-07); the safe
		// contract is to inject nothing and let run mode warn instead of guessing.
		if got := osHintArgs("codex", "darwin", "linux", "arm64"); got != nil {
			t.Errorf("codex has no injection flag; should inject no hint, got %v", got)
		}
	})
}
