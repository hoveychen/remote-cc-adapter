// rcc_seccomp.c — Linux seccomp + ptrace supervisor for remote-cc-adapter.
//
// This is the brain-side interception carrier for Linux (design doc §4.1.3 /
// §4.2). It exists for exactly one job: SUBPROCESS ROUTING.
//
// Bun on Linux issues raw syscalls that bypass glibc, so LD_PRELOAD symbol
// interposition misses its subprocess spawns (verified 2026-07-09: strace saw
// `claude doctor` raw-clone+execve /usr/bin/git, an LD_PRELOAD probe did not).
// seccomp-notify cannot rewrite syscall arguments, so routing needs ptrace. The
// filter returns SECCOMP_RET_TRACE for execve/execveat and the supervisor is
// also the tracer (PTRACE_O_TRACESECCOMP), so each execve stops the tracee at
// syscall entry with PTRACE_EVENT_SECCOMP. The supervisor applies the routing
// decision (mirrors native/macos/rcc_interpose.c my_posix_spawn) and, for remote
// routes, rewrites argv in tracee memory to
//   [RCC_SPAWN_PROXY, _spawn-proxy, <orig-path>, <orig-argv...>]
// so the spawn proxy (cmd/rca/spawnproxy.go) streams the process to the remote
// executor. Local routes continue unchanged.
//
// FILE I/O is NOT intercepted here. It used to be: openat was trapped as a
// SECCOMP_RET_USER_NOTIF and routed opens got a FUSE-backed fd injected. That
// gave the target a split view of its own working directory, because openat was
// trapped and stat/statx/getdents64/getcwd were not — `fstatat(dirfd, name)`
// answered from the remote while `stat(fullpath)` returned ENOENT from the local
// filesystem, and bun wedged at startup cross-checking the two. Trapping the
// whole stat family would have cost ~5.6k extra round trips per claude run with
// no kernel caching. Instead `rca _nsrun` mounts each remote directory at its
// own absolute path inside the target's private mount namespace, so the kernel
// gives every syscall one consistent view for free.
//
// Invocation (by the adapter, see internal/adapter/launch.go):
//   rcc_seccomp <target-binary> [args...]
// Environment: RCC_REMOTE_PREFIXES, RCC_SPAWN_PROXY, RCC_EXECUTOR_SOCK,
//   RCC_SPAWN_SENTINEL, RCC_CLAUDE_PATH, RCC_LOCAL_BINS. The spawn proxy inherits
//   the tracee's env (which already carries RCC_EXECUTOR_SOCK), so no envp
//   rewrite is needed.

#define _GNU_SOURCE
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <stdarg.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <sys/ptrace.h>
#include <sys/user.h>
#include <sys/uio.h>
#include <sys/types.h>
#include <signal.h>
#include <elf.h>
#include <linux/seccomp.h>
#include <linux/filter.h>
#include <linux/audit.h>

#ifndef PTRACE_EVENT_SECCOMP
#define PTRACE_EVENT_SECCOMP 7
#endif

// ---- audit log (opt-in via RCC_LOG=<path>) --------------------------------

static int LOGFD = -2; // -2 = unchecked, -1 = disabled
static void lg(const char *fmt, ...) {
  if (LOGFD == -1) return;
  if (LOGFD == -2) {
    const char *p = getenv("RCC_LOG");
    LOGFD = (p && *p) ? open(p, O_WRONLY | O_CREAT | O_APPEND, 0644) : -1;
    if (LOGFD < 0) { LOGFD = -1; return; }
  }
  char b[1024];
  va_list a;
  __builtin_va_start(a, fmt);
  int n = vsnprintf(b, sizeof b, fmt, a);
  __builtin_va_end(a);
  if (n > 0) { ssize_t w = write(LOGFD, b, n); (void)w; }
}

// ---- seccomp filter --------------------------------------------------------

static int seccomp(unsigned op, unsigned fl, void *a) { return syscall(__NR_seccomp, op, fl, a); }

// The filter traps execve/execveat as RET_TRACE (subprocess routing, handled via
// ptrace). Everything else — including all file I/O — runs unmodified.
// RET_TRACE encodes a data value in the low 16 bits; we use 1.
static int install_filter(void) {
  // BPF jump offsets are relative to the FOLLOWING instruction. Instruction
  // indices: 0 LD, 1..2 JEQ, 3 RET_ALLOW, 4 RET_TRACE.
  //   execve   (idx1): jt -> idx4; offset = 4-2 = 2
  //   execveat (idx2): jt -> idx4; offset = 4-3 = 1
  struct sock_filter f[] = {
      BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
      BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execve, 2, 0),
      BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execveat, 1, 0),
      BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),        // idx3
      BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_TRACE | 1),    // idx4: execve/at
  };
  struct sock_fprog p = {.len = sizeof f / sizeof f[0], .filter = f};
  prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0);
  return seccomp(SECCOMP_SET_MODE_FILTER, 0, &p);
}

// ---- tracee memory access --------------------------------------------------

// read_mem copies n bytes from tracee `pid` at `addr` into out. Returns bytes
// read (may be short at a page boundary), or -1.
static ssize_t read_mem(pid_t pid, unsigned long addr, void *out, size_t n) {
  struct iovec local = {.iov_base = out, .iov_len = n};
  struct iovec remote = {.iov_base = (void *)addr, .iov_len = n};
  return process_vm_readv(pid, &local, 1, &remote, 1, 0);
}

// read_cstr reads a NUL-terminated string from the tracee into a per-thread
// buffer. The supervisor is single-threaded now that the openat listener is
// gone, but the buffer stays thread-local: when it was shared, a second thread
// reading concurrently clobbered it, and the symptom (EXEC log lines whose arg0
// belonged to another syscall) took a while to trace back here.
static char *read_cstr(pid_t pid, unsigned long addr) {
  static _Thread_local char b[4096];
  if (!addr) return NULL;
  char mm[64];
  snprintf(mm, sizeof mm, "/proc/%d/mem", pid);
  int fd = open(mm, O_RDONLY);
  if (fd < 0) return NULL;
  ssize_t n = pread(fd, b, sizeof b - 1, addr);
  close(fd);
  if (n <= 0) return NULL;
  b[n] = 0;
  b[strnlen(b, sizeof b - 1)] = 0;
  return b;
}

// ---- routing ---------------------------------------------------------------

static int prefix_hit(const char *path, const char *env) {
  if (!path) return 0;
  const char *pre = getenv(env);
  if (!pre || !*pre) return 0;
  char *dup = strdup(pre);
  int hit = 0;
  for (char *tok = strtok(dup, ":"); tok && !hit; tok = strtok(NULL, ":")) {
    size_t l = strlen(tok);
    if (l == 0) continue;
    if (strncmp(path, tok, l) == 0 && (path[l] == '\0' || path[l] == '/')) hit = 1;
  }
  free(dup);
  return hit;
}

// is_remote: path is under one of RCC_REMOTE_PREFIXES (forward to executor).
static int is_remote(const char *path) { return prefix_hit(path, "RCC_REMOTE_PREFIXES"); }

// spawn_is_local_bin mirrors rcc_interpose.c: binaries that must always run
// locally even under a remote cwd. The spawn proxy itself (avoid an infinite
// loop) and claude re-spawning itself are matched by full path from the env.
// Terminal/clipboard tools that act on the operator's local session default to
// local (Linux equivalents of macOS pbcopy/tmux). Extend via RCC_LOCAL_BINS.
static int spawn_is_local_bin(const char *path) {
  if (!path) return 0;
  static const char *defaults[] = {"xclip", "xsel", "wl-copy", "wl-paste", "tmux", NULL};
  for (int i = 0; defaults[i]; i++)
    if (strstr(path, defaults[i])) return 1;
  const char *proxy = getenv("RCC_SPAWN_PROXY");
  if (proxy && *proxy && strstr(path, proxy)) return 1;
  const char *claude = getenv("RCC_CLAUDE_PATH");
  if (claude && *claude && strstr(path, claude)) return 1;
  const char *bins = getenv("RCC_LOCAL_BINS");
  if (bins && *bins) {
    char *dup = strdup(bins);
    int hit = 0;
    for (char *t = strtok(dup, ":"); t && !hit; t = strtok(NULL, ":"))
      if (*t && strstr(path, t)) hit = 1;
    free(dup);
    if (hit) return 1;
  }
  return 0;
}

// cwd_is_remote reads the TRACEE's cwd (/proc/<pid>/cwd), not the supervisor's,
// and reports whether it is under a remote prefix.
static int cwd_is_remote(pid_t pid) {
  char link[64], cwd[4096];
  snprintf(link, sizeof link, "/proc/%d/cwd", pid);
  ssize_t n = readlink(link, cwd, sizeof cwd - 1);
  if (n <= 0) return 0;
  cwd[n] = 0;
  return is_remote(cwd);
}

// route_spawn decides whether an exec of `path` with argv `av` (n entries) in
// tracee `pid` should run on the remote executor. Precedence mirrors
// rcc_interpose.c my_posix_spawn (highest first):
//   1. rg-mode self-invocation (--no-config) with a remote cwd -> REMOTE
//   2. local-binary allowlist -> LOCAL (proxy/self-spawn/clipboard/tmux)
//   3. RCC_SPAWN_SENTINEL present in argv -> REMOTE
//   4. target binary under a remote prefix -> REMOTE
//   5. working directory under a remote prefix -> REMOTE
//   6. otherwise -> LOCAL
static int route_spawn(pid_t pid, const char *path, char **av, int n, const char **reason) {
  const char *sentinel = getenv("RCC_SPAWN_SENTINEL");

  int rgmode = 0;
  for (int j = 1; j < n; j++)
    if (av[j] && strcmp(av[j], "--no-config") == 0) { rgmode = 1; break; }

  int cwdrem = cwd_is_remote(pid);

  if (spawn_is_local_bin(path) && !(rgmode && cwdrem)) { *reason = "local-allowlist"; return 0; }

  int sent = 0;
  if (sentinel && *sentinel)
    for (int j = 0; j < n; j++)
      if (av[j] && strstr(av[j], sentinel)) { sent = 1; break; }

  if (rgmode && cwdrem) { *reason = "rg-mode"; return 1; }
  if (sent) { *reason = "sentinel"; return 1; }
  if (is_remote(path)) { *reason = "target-prefix"; return 1; }
  if (cwdrem) { *reason = "cwd-prefix"; return 1; }
  *reason = "default-local";
  return 0;
}

// ---- execve argv rewrite (ptrace path) -------------------------------------

#if defined(__x86_64__)

// read_regs / write_regs via the arch-neutral PTRACE_GETREGSET(NT_PRSTATUS).
static int read_regs(pid_t pid, struct user_regs_struct *r) {
  struct iovec io = {.iov_base = r, .iov_len = sizeof *r};
  return ptrace(PTRACE_GETREGSET, pid, (void *)NT_PRSTATUS, &io);
}
static int write_regs(pid_t pid, struct user_regs_struct *r) {
  struct iovec io = {.iov_base = r, .iov_len = sizeof *r};
  return ptrace(PTRACE_SETREGSET, pid, (void *)NT_PRSTATUS, &io);
}

#define MAX_ARGV 1024

// rewrite_execve inspects the pending execve of tracee `pid` and, if it routes
// remote, rewrites argv to [proxy, _spawn-proxy, orig-path, orig-argv...]. It
// only rewrites plain execve (nr 59); execveat is left to run locally. Returns
// 0 (nothing to do / rewritten in place); the caller then PTRACE_CONTs.
//
// Rewrite strategy (validated by POC2): the kernel copies the path, argv strings
// and envp out of the OLD address space when execve runs, so the original argv
// string pointers stay valid at exec time. We therefore only need to inject two
// new strings (the proxy path and "_spawn-proxy") plus a fresh pointer array
// into scratch space below rsp (the stack mapping is live), then point rdi at
// the proxy string and rsi at the new pointer array. envp (rdx) is untouched —
// the tracee's env already carries RCC_EXECUTOR_SOCK.
static void rewrite_execve(pid_t pid) {
  struct user_regs_struct regs;
  if (read_regs(pid, &regs) != 0) return;
  if (regs.orig_rax != __NR_execve) {
    lg("EXEC\tnr=%lld (not execve, run local)\n", (long long)regs.orig_rax);
    return; // execveat and friends: run local
  }

  unsigned long path_addr = regs.rdi;
  unsigned long argv_addr = regs.rsi;

  char pathbuf[4096];
  char *p = read_cstr(pid, path_addr);
  if (!p) return;
  strncpy(pathbuf, p, sizeof pathbuf - 1);
  pathbuf[sizeof pathbuf - 1] = 0;

  // Read the original argv pointer array (absolute tracee addresses).
  unsigned long orig_argv[MAX_ARGV];
  int n = 0;
  for (; n < MAX_ARGV; n++) {
    unsigned long ptr = 0;
    if (read_mem(pid, argv_addr + (unsigned long)n * 8, &ptr, 8) != 8) break;
    if (ptr == 0) break;
    orig_argv[n] = ptr;
  }

  const char *proxy = getenv("RCC_SPAWN_PROXY");
  if (!proxy || !*proxy) return; // no proxy configured: run local

  const char *reason = "?";
  // Build a char** view of argv for the router (strings read on demand).
  char *avstr[MAX_ARGV];
  static _Thread_local char argv_str_pool[MAX_ARGV][256];
  for (int i = 0; i < n; i++) {
    char *s = read_cstr(pid, orig_argv[i]);
    if (s) { strncpy(argv_str_pool[i], s, 255); argv_str_pool[i][255] = 0; avstr[i] = argv_str_pool[i]; }
    else avstr[i] = (char *)"";
  }
  int route = route_spawn(pid, pathbuf, avstr, n, &reason);
  lg("EXEC\tpath=%s\troute=%d\treason=%s\targ0=%s\n", pathbuf, route, reason, n > 0 ? avstr[0] : "");
  if (!route) return; // local: run unmodified

  // ---- assemble the scratch region below rsp -------------------------------
  // Layout: [proxy-str][\0]["_spawn-proxy"][\0] (align 8) [ptr-array...]
  const char *sp = "_spawn-proxy";
  size_t proxy_len = strlen(proxy) + 1;
  size_t sp_len = strlen(sp) + 1;

  unsigned long scratch = (regs.rsp - 0x4000) & ~0xfUL;
  unsigned long off = 0;
  unsigned long proxy_at = scratch + off; off += proxy_len;
  unsigned long sp_at = scratch + off; off += sp_len;
  off = (off + 7) & ~7UL; // align the pointer array to 8 bytes

  unsigned long arr_at = scratch + off;
  // new argv: proxy, _spawn-proxy, orig-path, orig-argv[0..n-1], NULL
  int total = 3 + n + 1;
  if (total > MAX_ARGV + 8) return;
  unsigned long arr[MAX_ARGV + 8];
  int k = 0;
  arr[k++] = proxy_at;
  arr[k++] = sp_at;
  arr[k++] = path_addr; // orig path string is still mapped at exec time
  for (int i = 0; i < n; i++) arr[k++] = orig_argv[i];
  arr[k++] = 0;

  // One buffer holding the whole scratch region, written in a single shot.
  size_t region = off + (size_t)k * 8;
  char *buf = calloc(1, region);
  if (!buf) return;
  memcpy(buf + (proxy_at - scratch), proxy, proxy_len);
  memcpy(buf + (sp_at - scratch), sp, sp_len);
  memcpy(buf + (arr_at - scratch), arr, (size_t)k * 8);

  struct iovec local = {.iov_base = buf, .iov_len = region};
  struct iovec remote = {.iov_base = (void *)scratch, .iov_len = region};
  ssize_t w = process_vm_writev(pid, &local, 1, &remote, 1, 0);
  free(buf);
  if (w != (ssize_t)region) { lg("EXEC\trewrite write failed (%zd/%zu)\n", w, region); return; }

  regs.rdi = proxy_at;
  regs.rsi = arr_at;
  // rdx (envp) unchanged.
  if (write_regs(pid, &regs) != 0) { lg("EXEC\tsetregs failed\n"); return; }
  lg("EXEC\trewrote -> %s _spawn-proxy %s (%d args)\n", proxy, pathbuf, n);
}

#else
static void rewrite_execve(pid_t pid) { (void)pid; } // non-x86_64: no rewrite
#endif

// ---- supervisor + tracer ---------------------------------------------------

int main(int argc, char **argv) {
  if (argc < 2) { fprintf(stderr, "usage: rcc_seccomp <target> [args...]\n"); return 2; }

  pid_t pid = fork();
  if (pid == 0) {
    // Become traceable by the supervisor, install the filter, then stop so the
    // supervisor can set ptrace options before our execvp (which will trap as
    // PTRACE_EVENT_SECCOMP and route).
    ptrace(PTRACE_TRACEME, 0, 0, 0);
    if (install_filter() != 0) { perror("seccomp install"); _exit(97); }
    kill(getpid(), SIGSTOP);
    execvp(argv[1], &argv[1]);
    perror("exec");
    _exit(96);
  }
  if (pid < 0) { perror("fork"); return 1; }

  // Wait for the child's post-TRACEME SIGSTOP, then arm ptrace: trap seccomp
  // RET_TRACE events (execve) and auto-attach fork/vfork/clone children so
  // subprocesses of subprocesses are traced too. EXITKILL tears the tree down
  // if the supervisor dies.
  int st;
  if (waitpid(pid, &st, 0) != pid) { perror("waitpid initial"); return 1; }
  long opts = PTRACE_O_TRACESECCOMP | PTRACE_O_TRACEFORK | PTRACE_O_TRACEVFORK |
              PTRACE_O_TRACECLONE | PTRACE_O_EXITKILL;
  if (ptrace(PTRACE_SETOPTIONS, pid, 0, (void *)opts) != 0) perror("setoptions");

  ptrace(PTRACE_CONT, pid, 0, 0);

  int exit_code = 0;
  // Blocking tracer loop. __WALL reaps threads (clone) too.
  for (;;) {
    int status;
    pid_t w = waitpid(-1, &status, __WALL);
    if (w < 0) {
      if (errno == EINTR) continue;
      break; // ECHILD: no tracees left
    }

    if (WIFEXITED(status) || WIFSIGNALED(status)) {
      if (w == pid) {
        exit_code = WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
        break;
      }
      continue; // a sub-tracee exited; nothing to do
    }

    if (WIFSTOPPED(status)) {
      int sig = WSTOPSIG(status);
      int event = (status >> 16) & 0xff;
      if (event == PTRACE_EVENT_SECCOMP) {
        rewrite_execve(w);              // decide + maybe rewrite argv
        ptrace(PTRACE_CONT, w, 0, 0);
      } else if (event == PTRACE_EVENT_FORK || event == PTRACE_EVENT_VFORK ||
                 event == PTRACE_EVENT_CLONE) {
        ptrace(PTRACE_CONT, w, 0, 0);   // new child auto-attached; let both run
      } else if (sig == SIGTRAP) {
        ptrace(PTRACE_CONT, w, 0, 0);   // stray trap (e.g. initial exec): swallow
      } else if (sig == SIGSTOP || sig == SIGTSTP || sig == SIGTTIN || sig == SIGTTOU) {
        ptrace(PTRACE_CONT, w, 0, 0);   // group-stop: resume, don't reinject
      } else {
        // signal-delivery-stop: reinject. The signal goes in ptrace's DATA (4th)
        // argument; addr is ignored for PTRACE_CONT. Passing it as addr (3rd) —
        // as this line used to — delivered signal 0, i.e. silently dropped every
        // reinjected signal, which wedged JSC's SIGPWR stop-the-world GC.
        ptrace(PTRACE_CONT, w, 0, (void *)(long)sig);
      }
    }
  }

  return exit_code;
}
