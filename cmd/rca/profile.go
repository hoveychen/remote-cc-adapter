package main

// Engine profiles describe, per supported agent CLI, which paths hold the
// engine's own "brain state" (credentials, config, session history) that must
// stay on the local host under --default-remote (ModeLocalAllowlist). Without
// pinning these, a default-remote launch would forward the engine's own
// credential/session reads to the remote executor and it could not boot.
//
// The routing layer itself is engine-agnostic — it only knows path prefixes and
// a mode. Profiles are the one place engine-specific knowledge lives, so adding
// a new engine is a table entry, not a code change.

import (
	"fmt"
	"os"
	"path/filepath"
)

// engineProfile captures where an engine keeps its local-only state.
//
// The config home resolves to $envHome when that env var is set (and non-empty),
// otherwise to ~/defaultDir. extraFiles are additional ~/-relative paths pinned
// unconditionally alongside the config home (e.g. a top-level dot-file that lives
// outside the config dir). absExtras are absolute, HOME-independent paths — the
// engine's system-wide config folders (managed/enterprise policy) that it reads
// "if present"; pinning a path that does not exist is harmless.
type engineProfile struct {
	envHome    string   // env var that overrides the config home dir ("" if none)
	defaultDir string   // ~/-relative default config dir, e.g. ".claude"
	extraFiles []string // additional ~/-relative files to pin local
	absExtras  []string // absolute system paths to pin local (managed config, etc.)
	// spawnHelpers are sibling binaries the engine spawns that live next to its
	// own executable and must run on the local (brain) host — not the remote
	// executor — even when the working directory routes remote. The engine
	// resolves them relative to its own argv[0] dir, so on macOS the adapter
	// copies+re-signs each one next to the target's re-signed copy (see
	// cmd/rca/run.go). The interceptor keeps them local by basename (see
	// spawn_is_local_bin in native/macos/rcc_interpose.c). Names are bare
	// basenames resolved against the target binary's directory.
	spawnHelpers []string

	// osHintFlag is the CLI flag that appends extra text to the engine's system
	// prompt, used to tell the agent that its routed subprocesses execute on the
	// executor's OS rather than the local host it perceives. Empty when the
	// engine exposes no such flag (then no arg-based hint is injected).
	osHintFlag string
}

// engineProfiles maps an engine name (the target command's basename) to its
// profile. Verified against each engine's own config-home resolution and its
// system-config lookup:
//   - claude: envUtils.getClaudeConfigHomeDir = CLAUDE_CONFIG_DIR ?? ~/.claude,
//     plus the separate global config file ~/.claude.json, plus the system-wide
//     managed-settings.json (/etc/claude-code on Linux, /Library/Application
//     Support/ClaudeCode on macOS — both pinned; the non-matching one is inert).
//   - codex:  find_codex_home() = CODEX_HOME ?? ~/.codex; ALL user codex state
//     (auth.json, config.toml, history.jsonl, sessions/, memories/, skills/,
//     agents/, rules/, log/, caches, tmp/arg0) lives under that single dir, so
//     there is no separate top-level dot-file. On Unix codex also reads the
//     system config folder /etc/codex (config.toml, managed_config.toml,
//     requirements.toml, skills/, rules/) "if present" — pinned via absExtras.
var engineProfiles = map[string]engineProfile{
	"claude": {
		envHome:    "CLAUDE_CONFIG_DIR",
		defaultDir: ".claude",
		extraFiles: []string{".claude.json"},
		absExtras: []string{
			"/etc/claude-code/managed-settings.json",
			"/Library/Application Support/ClaudeCode/managed-settings.json",
		},
		osHintFlag: "--append-system-prompt",
	},
	"codex": {
		envHome:    "CODEX_HOME",
		defaultDir: ".codex",
		absExtras:  []string{"/etc/codex"},
		// osHintFlag intentionally empty: codex has no --append-system-prompt (or
		// --system-prompt) flag as of 2026-07 (verified against the published CLI
		// reference; the feature is an open upstream request, openai/codex#11117).
		// Its only persistent instruction surface is AGENTS.md, but injecting that
		// means writing into the user's project dir (which routes remote and could
		// clobber an existing file) or their global ~/.codex — both intrusive and
		// unverified here. So on a cross-OS codex launch run mode logs an
		// actionable warning instead of silently doing the wrong thing; wire a
		// verified mechanism in when codex ships an append flag.
		// codex 0.144.4+ "code mode" runs shell commands through a persistent
		// sibling helper, codex-code-mode-host, which it execs from its own
		// bin dir; that helper then spawns the actual shell (/bin/sh -lc ...).
		// The helper must stay local (it's a host-arch binary) so its shell
		// children route remote by cwd like any other subprocess.
		spawnHelpers: []string{"codex-code-mode-host"},
	},
}

// detectProfile picks an engine profile from the target command. The command may
// be a bare name ("codex") or a path ("/usr/local/bin/codex"); we match on the
// basename. Returns "" when no profile matches — the caller then relies on the
// operator's explicit --local-prefix flags.
func detectProfile(command string) string {
	base := filepath.Base(command)
	if _, ok := engineProfiles[base]; ok {
		return base
	}
	return ""
}

// profileLocalPrefixes returns the local-pin prefixes for the named engine
// profile, resolved against the current environment (HOME and the profile's
// config-home env var). An unknown profile, or one with nothing to anchor to
// (e.g. HOME unset), yields no prefixes.
func profileLocalPrefixes(profile string) []string {
	p, ok := engineProfiles[profile]
	if !ok {
		return nil
	}
	home := os.Getenv("HOME")

	var out []string
	// Config home: the env override wins; otherwise ~/defaultDir. Matching each
	// engine's own resolution, the env var *replaces* the default dir rather than
	// adding to it.
	if p.envHome != "" {
		if v := os.Getenv(p.envHome); v != "" {
			out = append(out, v)
		} else if home != "" && p.defaultDir != "" {
			out = append(out, filepath.Join(home, p.defaultDir))
		}
	} else if home != "" && p.defaultDir != "" {
		out = append(out, filepath.Join(home, p.defaultDir))
	}

	// Extra dot-files always anchor to HOME, independent of the config-home env.
	if home != "" {
		for _, f := range p.extraFiles {
			out = append(out, filepath.Join(home, f))
		}
	}

	// Absolute system paths (managed/enterprise config) pin regardless of HOME.
	out = append(out, p.absExtras...)
	return out
}

// profileSpawnHelpers returns the bare basenames of sibling helper binaries the
// named engine spawns that must run on the local host. Empty for an unknown
// profile or an engine with no such helpers.
func profileSpawnHelpers(profile string) []string {
	p, ok := engineProfiles[profile]
	if !ok {
		return nil
	}
	return append([]string(nil), p.spawnHelpers...)
}

// osHintArgs returns argv to prepend to the engine's own arguments so the agent
// targets the executor's platform instead of the local host it perceives. It
// returns nil when there is nothing to inject: the executor OS is unknown or
// matches the local OS (no mismatch), or the engine exposes no injection flag.
//
// Rationale: run mode launches the agent on the local host, so the agent's
// environment block reports the local OS. But its Bash/tool subprocesses route
// to the executor. When the two hosts differ (e.g. macOS agent, Linux
// executor), the agent otherwise emits BSD-flavoured commands that fail on the
// executor's GNU userland, causing repeated tool errors.
func osHintArgs(profile, localOS, execOS, execArch string) []string {
	if execOS == "" || execOS == localOS {
		return nil
	}
	p, ok := engineProfiles[profile]
	if !ok || p.osHintFlag == "" {
		return nil
	}
	return []string{p.osHintFlag, osHintText(execOS, execArch)}
}

// osHintText is the system-prompt addendum describing the real execution host.
// execOS/execArch are Go GOOS/GOARCH values (e.g. "linux"/"arm64").
func osHintText(execOS, execArch string) string {
	return fmt.Sprintf("IMPORTANT — remote execution environment: your shell commands "+
		"(the Bash tool and every subprocess it spawns) run on a REMOTE %s/%s host, not on "+
		"the machine described in your environment context. Write commands for %s: use its "+
		"userland conventions (e.g. GNU coreutils flags on linux, BSD on darwin), its file "+
		"paths, and only binaries installed on the %s host. Ignore the local platform shown "+
		"in your environment block when choosing command syntax.", execOS, execArch, execOS, execOS)
}
