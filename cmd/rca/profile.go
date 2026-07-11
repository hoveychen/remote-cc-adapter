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
	},
	"codex": {
		envHome:    "CODEX_HOME",
		defaultDir: ".codex",
		absExtras:  []string{"/etc/codex"},
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
