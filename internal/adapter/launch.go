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
	EnvSpawnProxy    = "RCC_SPAWN_PROXY"     // rca binary; routed spawns exec `rca _spawn-proxy ...`
	EnvRemotePrefix  = "RCC_REMOTE_PREFIXES" // ':'-joined remote-routed path prefixes
	EnvSpawnSentinel = "RCC_SPAWN_SENTINEL"  // marker that forces a subprocess remote
	EnvTargetPath    = "RCC_TARGET_PATH"     // target agent binary path; kept local when re-spawned
	EnvDylib         = "DYLD_INSERT_LIBRARIES"
)

// LaunchConfig describes how to spawn the intercepted target agent process.
type LaunchConfig struct {
	// TargetPath is the target agent binary to run (e.g. claude, codex). On macOS
	// the adapter runs a re-signed copy (see PrepareMacOSCopy); on Linux it is the
	// real binary launched under the seccomp supervisor.
	TargetPath string
	// Args are the arguments passed to the target.
	Args []string
	// WorkDir sets the target's working directory. Point it under a remote prefix
	// to exercise natural cwd-based subprocess routing. Empty inherits the adapter's.
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

	// ExtraEnv is appended to the child environment (KEY=VALUE).
	ExtraEnv []string
}

// BuildCommand assembles the *exec.Cmd that launches the intercepted target
// agent process for the current platform. It does not start the process.
func (c *LaunchConfig) BuildCommand() (*exec.Cmd, error) {
	if c.TargetPath == "" {
		return nil, fmt.Errorf("adapter: TargetPath is required")
	}
	env := append(os.Environ(),
		EnvAdapterSock+"="+c.AdapterSock,
		EnvExecutorSock+"="+c.ExecutorSock,
		EnvSpawnProxy+"="+c.SpawnProxyPath,
		EnvRemotePrefix+"="+strings.Join(c.RemotePrefixes, ":"),
		EnvSpawnSentinel+"="+c.SpawnSentinel,
		EnvTargetPath+"="+c.TargetPath,
	)
	env = append(env, c.ExtraEnv...)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		if c.DylibPath == "" {
			return nil, fmt.Errorf("adapter: DylibPath is required on macOS")
		}
		cmd = exec.Command(c.TargetPath, c.Args...)
		env = append(env, EnvDylib+"="+c.DylibPath)
	case "linux":
		if c.SupervisorPath == "" {
			return nil, fmt.Errorf("adapter: SupervisorPath is required on Linux")
		}
		// The supervisor installs the seccomp filter, then execs the target. Routed
		// paths are served by the FUSE mounts rca _nsrun sets up around it.
		args := append([]string{c.TargetPath}, c.Args...)
		cmd = exec.Command(c.SupervisorPath, args...)
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

// PrepareMacOSCopy copies the real target agent binary to dest and ad-hoc
// re-signs it so an external interpose dylib can be loaded (design doc §4.2). The
// original signature is dropped in the process; the copy is for local
// interception only and must never be redistributed. Returns dest on success.
//
// This mutates only files under dest's directory; it never touches the real
// installed target binary.
func PrepareMacOSCopy(realTarget, dest string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("adapter: PrepareMacOSCopy is macOS-only")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := copyFile(realTarget, dest, 0o755); err != nil {
		return "", fmt.Errorf("copy target binary: %w", err)
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
