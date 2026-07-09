package adapter

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variables the adapter passes to the injected claude process. The
// native interceptor (dylib / seccomp supervisor) and the spawn proxy read
// these to find their sockets and routing configuration.
const (
	EnvAdapterSock   = "RCC_ADAPTER_SOCK"    // fs IO-RPC socket the interceptor dials
	EnvExecutorSock  = "RCC_EXECUTOR_SOCK"   // executor socket the spawn proxy dials
	EnvSpawnProxy    = "RCC_SPAWN_PROXY"     // path to the rcc-spawn-proxy binary
	EnvRemotePrefix  = "RCC_REMOTE_PREFIXES" // ':'-joined remote-routed path prefixes
	EnvSpawnSentinel = "RCC_SPAWN_SENTINEL"  // marker that forces a subprocess remote
	EnvClaudePath    = "RCC_CLAUDE_PATH"     // claude binary path; kept local when re-spawned
	EnvFuseMnt       = "RCC_FUSE_MNT"        // Linux: FUSE mount the seccomp supervisor redirects opens to
	EnvDylib         = "DYLD_INSERT_LIBRARIES"
)

// LaunchConfig describes how to spawn the intercepted claude process.
type LaunchConfig struct {
	// ClaudePath is the claude binary to run. On macOS the adapter runs a
	// re-signed copy (see PrepareMacOSCopy); on Linux it is the real binary
	// launched under the seccomp supervisor.
	ClaudePath string
	// Args are the arguments passed to claude.
	Args []string
	// WorkDir sets claude's working directory. Point it under a remote prefix to
	// exercise natural cwd-based subprocess routing. Empty inherits the adapter's.
	WorkDir string

	AdapterSock    string
	ExecutorSock   string
	SpawnProxyPath string
	RemotePrefixes []string
	SpawnSentinel  string

	// DylibPath is the macOS interpose dylib (ignored on Linux).
	DylibPath string
	// SupervisorPath is the Linux seccomp supervisor binary (ignored on macOS).
	SupervisorPath string
	// FuseMnt is the Linux rcc-fuse mount point the supervisor redirects routed
	// opens to (ignored on macOS).
	FuseMnt string

	// ExtraEnv is appended to the child environment (KEY=VALUE).
	ExtraEnv []string
}

// BuildCommand assembles the *exec.Cmd that launches the intercepted claude
// process for the current platform. It does not start the process.
func (c *LaunchConfig) BuildCommand() (*exec.Cmd, error) {
	if c.ClaudePath == "" {
		return nil, fmt.Errorf("adapter: ClaudePath is required")
	}
	env := append(os.Environ(),
		EnvAdapterSock+"="+c.AdapterSock,
		EnvExecutorSock+"="+c.ExecutorSock,
		EnvSpawnProxy+"="+c.SpawnProxyPath,
		EnvRemotePrefix+"="+strings.Join(c.RemotePrefixes, ":"),
		EnvSpawnSentinel+"="+c.SpawnSentinel,
		EnvClaudePath+"="+c.ClaudePath,
	)
	env = append(env, c.ExtraEnv...)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		if c.DylibPath == "" {
			return nil, fmt.Errorf("adapter: DylibPath is required on macOS")
		}
		cmd = exec.Command(c.ClaudePath, c.Args...)
		env = append(env, EnvDylib+"="+c.DylibPath)
	case "linux":
		if c.SupervisorPath == "" {
			return nil, fmt.Errorf("adapter: SupervisorPath is required on Linux")
		}
		if c.FuseMnt == "" {
			return nil, fmt.Errorf("adapter: FuseMnt is required on Linux (the rcc-fuse mount)")
		}
		// The supervisor installs the seccomp filter, then execs claude; it
		// redirects routed opens to the rcc-fuse mount.
		args := append([]string{c.ClaudePath}, c.Args...)
		cmd = exec.Command(c.SupervisorPath, args...)
		env = append(env, EnvFuseMnt+"="+c.FuseMnt)
	default:
		return nil, fmt.Errorf("adapter: unsupported platform %q", runtime.GOOS)
	}
	cmd.Env = env
	cmd.Dir = c.WorkDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// PrepareMacOSCopy copies the real claude binary to dest and ad-hoc re-signs it
// so an external interpose dylib can be loaded (design doc §4.2). The original
// Anthropic signature is dropped in the process; the copy is for local
// interception only and must never be redistributed. Returns dest on success.
//
// This mutates only files under dest's directory; it never touches the real
// installed claude binary.
func PrepareMacOSCopy(realClaude, dest string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("adapter: PrepareMacOSCopy is macOS-only")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := copyFile(realClaude, dest, 0o755); err != nil {
		return "", fmt.Errorf("copy claude: %w", err)
	}
	// Ad-hoc re-sign, dropping hardened runtime so DYLD_INSERT_LIBRARIES works.
	// disable-library-validation is already set in the original entitlements.
	sign := exec.Command("codesign", "--force", "--sign", "-", dest)
	if out, err := sign.CombinedOutput(); err != nil {
		return "", fmt.Errorf("codesign: %w: %s", err, out)
	}
	return dest, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
