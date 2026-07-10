// rcc_seccomp.c — Linux seccomp-user-notify + ptrace supervisor for
// remote-cc-adapter.
//
// This is the brain-side interception載体 for Linux (design doc §4.1.3 / §4.2).
// Unlike macOS, Bun on Linux issues raw syscalls that bypass glibc, so
// LD_PRELOAD symbol interposition misses both its file I/O AND its subprocess
// spawns; a seccomp filter traps at the syscall boundary instead and catches
// everything.
//
// Two interception paths, multiplexed in one supervisor process:
//
//   1. FILE I/O (openat) — SECCOMP_RET_USER_NOTIF. The child installs a filter
//      that turns openat into a USER_NOTIF and hands the listener fd to the
//      supervisor over a socketpair (SCM_RIGHTS). The supervisor reads the path
//      from /proc/<pid>/mem and, for routed paths, injects a FUSE-backed fd via
//      NOTIF_ADDFD(FLAG_SEND); non-routed opens get FLAG_CONTINUE. (ported from
//      the validated POC seccomp/sec.c — DO NOT change this path, it is what
//      makes lazy-slice file reads work, design doc §4.1.3 / §4.3.)
//
//   2. SUBPROCESS (execve/execveat) — SECCOMP_RET_TRACE. LD_PRELOAD can't see
//      bun's raw clone+execve (verified 2026-07-09: strace saw the execve but an
//      LD_PRELOAD probe did not), and seccomp-notify cannot rewrite syscall args,
//      so subprocess routing needs ptrace. The same filter returns
//      SECCOMP_RET_TRACE for execve/execveat; the supervisor is also the tracer
//      (PTRACE_O_TRACESECCOMP), so each execve stops the tracee at syscall entry
//      with PTRACE_EVENT_SECCOMP. The supervisor applies the routing decision
//      (mirrors native/macos/rcc_interpose.c my_posix_spawn) and, for remote
//      routes, rewrites argv in tracee memory to
//        [RCC_SPAWN_PROXY, _spawn-proxy, <orig-path>, <orig-argv...>]
//      so the spawn proxy (cmd/rca/spawnproxy.go) streams the process to the
//      remote executor. Local routes continue unchanged.
//
// Multiplexing (the hard part): the two mechanisms use incompatible wait
// primitives — the openat listener fd wants NOTIF_RECV, the ptrace stops want
// waitpid — and signalfd does NOT reliably observe ptrace-stop SIGCHLDs
// (verified 2026-07-09 on us-303: a poll() on signalfd+listener slept forever
// while the tracee sat in a traced-stop). ptrace also binds each tracee to the
// exact thread that attached it (the fork()ing main thread), so waitpid for a
// tracee must run there. The supervisor therefore splits the two: the MAIN
// thread is the tracer and runs a blocking waitpid(-1, __WALL) loop (all ptrace
// calls live here); a second thread runs a blocking NOTIF_RECV loop on the
// listener fd (ioctl is not thread-bound). The process exits when the main
// tracee exits, which tears the openat thread down with it.
//
// Invocation (by the adapter, see internal/adapter/launch.go):
//   rcc_seccomp <target-binary> [args...]
// Environment: RCC_FUSE_MNT, RCC_REMOTE_PREFIXES (file routing);
//   RCC_SPAWN_PROXY, RCC_EXECUTOR_SOCK, RCC_SPAWN_SENTINEL, RCC_CLAUDE_PATH,
//   RCC_LOCAL_BINS (subprocess routing). The spawn proxy inherits the tracee's
//   env (which already carries RCC_EXECUTOR_SOCK), so no envp rewrite is needed.

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
#include <sys/ioctl.h>
#include <sys/syscall.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <sys/ptrace.h>
#include <sys/user.h>
#include <sys/uio.h>
#include <sys/types.h>
#include <signal.h>
#include <pthread.h>
#include <elf.h>
#include <linux/seccomp.h>
#include <linux/filter.h>
#include <linux/audit.h>

#ifndef SECCOMP_ADDFD_FLAG_SEND
#define SECCOMP_ADDFD_FLAG_SEND (1UL << 1)
#endif
#ifndef SECCOMP_USER_NOTIF_FLAG_CONTINUE
#define SECCOMP_USER_NOTIF_FLAG_CONTINUE (1UL << 0)
#endif
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

// The filter traps openat as a USER_NOTIF (file I/O, handled in-supervisor) and
// execve/execveat as RET_TRACE (subprocess, handled via ptrace). Everything else
// runs unmodified. RET_TRACE encodes a data value in the low 16 bits; we use 1.
static int install_filter(void) {
  // BPF jump offsets are relative to the FOLLOWING instruction. Instruction
  // indices: 0 LD, 1..3 JEQ, 4 RET_ALLOW, 5 RET_USER_NOTIF, 6 RET_TRACE.
  //   openat   (idx1): jt -> idx5 (USER_NOTIF); offset = 5-2 = 3
  //   execve   (idx2): jt -> idx6 (TRACE);      offset = 6-3 = 3
  //   execveat (idx3): jt -> idx6 (TRACE);      offset = 6-4 = 2
  struct sock_filter f[] = {
      BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
      BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat, 3, 0),
      BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execve, 3, 0),
      BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_execveat, 2, 0),
      BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),                 // idx4
      BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_USER_NOTIF),           // idx5: openat
      BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_TRACE | 1),           // idx6: execve/at
  };
  struct sock_fprog p = {.len = sizeof f / sizeof f[0], .filter = f};
  prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0);
  return seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_NEW_LISTENER, &p);
}

static int send_fd(int sock, int fd) {
  struct msghdr m = {0};
  char cbuf[CMSG_SPACE(sizeof(int))] = {0};
  struct iovec io = {.iov_base = "x", .iov_len = 1};
  m.msg_iov = &io; m.msg_iovlen = 1; m.msg_control = cbuf; m.msg_controllen = sizeof cbuf;
  struct cmsghdr *c = CMSG_FIRSTHDR(&m);
  c->cmsg_level = SOL_SOCKET; c->cmsg_type = SCM_RIGHTS; c->cmsg_len = CMSG_LEN(sizeof(int));
  memcpy(CMSG_DATA(c), &fd, sizeof(int));
  return sendmsg(sock, &m, 0);
}
static int recv_fd(int sock) {
  struct msghdr m = {0};
  char cbuf[CMSG_SPACE(sizeof(int))] = {0};
  char d; struct iovec io = {.iov_base = &d, .iov_len = 1};
  m.msg_iov = &io; m.msg_iovlen = 1; m.msg_control = cbuf; m.msg_controllen = sizeof cbuf;
  if (recvmsg(sock, &m, 0) < 0) return -1;
  struct cmsghdr *c = CMSG_FIRSTHDR(&m);
  int fd; memcpy(&fd, CMSG_DATA(c), sizeof(int));
  return fd;
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
// buffer. The buffer MUST be thread-local: the openat NOTIF thread holds the
// returned pointer across is_remote() and open_fuse() (which blocks on a real
// open() through the FUSE mount), while the tracer thread reads execve paths and
// argv strings concurrently. A shared buffer let one clobber the other — visible
// as EXEC log lines whose arg0 belonged to some other syscall, and able to make
// open_fuse() open a different remote path than the one that was routed.
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

// read_path is read_cstr under the name the openat handler uses. Isolation from
// the execve handler comes from read_cstr's buffer being thread-local, not from
// a second buffer.
static char *read_path(pid_t pid, unsigned long addr) { return read_cstr(pid, addr); }

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

// ---- FUSE-backed redirection (openat path) ---------------------------------

// open_fuse opens the routed path's FUSE-backed file under RCC_FUSE_MNT and
// returns the fd (caller injects it). The entry name is hex(path), matching
// linuxfuse.EncodePath — hex avoids '/' in the FUSE entry name.
static int open_fuse(const char *path) {
  const char *mnt = getenv("RCC_FUSE_MNT");
  if (!mnt || !*mnt) return -1;
  size_t pl = strlen(path), ml = strlen(mnt);
  char *fp = malloc(ml + 1 + pl * 2 + 1);
  if (!fp) return -1;
  memcpy(fp, mnt, ml);
  size_t k = ml;
  fp[k++] = '/';
  static const char *H = "0123456789abcdef";
  for (size_t i = 0; i < pl; i++) {
    unsigned char c = (unsigned char)path[i];
    fp[k++] = H[c >> 4];
    fp[k++] = H[c & 0xf];
  }
  fp[k] = 0;
  int fd = open(fp, O_RDONLY);
  free(fp);
  return fd;
}

// handle_openat services one openat USER_NOTIF: routed paths get a FUSE-backed
// fd injected (ADDFD_FLAG_SEND completes the notification); everything else gets
// FLAG_CONTINUE (the kernel runs the real openat).
static void handle_openat(int lf, struct seccomp_notif *req, struct seccomp_notif_resp *resp) {
  char *path = read_path(req->pid, req->data.args[1]);
  if (path && is_remote(path)) {
    int ffd = open_fuse(path);
    if (ffd >= 0) {
      struct seccomp_notif_addfd af = {0};
      af.id = req->id; af.flags = SECCOMP_ADDFD_FLAG_SEND; af.srcfd = ffd; af.newfd = 0;
      if (ioctl(lf, SECCOMP_IOCTL_NOTIF_ADDFD, &af) == 0) { close(ffd); return; }
      close(ffd);
    }
  }
  memset(resp, 0, sizeof *resp);
  resp->id = req->id;
  resp->flags = SECCOMP_USER_NOTIF_FLAG_CONTINUE;
  ioctl(lf, SECCOMP_IOCTL_NOTIF_SEND, resp);
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

// ---- openat listener thread ------------------------------------------------

// The openat USER_NOTIF path runs in its own thread: a blocking NOTIF_RECV loop.
// It touches only the listener fd and FUSE (no ptrace), so it is safe off the
// tracer thread. It exits when NOTIF_RECV fails (the tracee tree is gone) or
// when the process exits under it.
static void *openat_thread(void *arg) {
  int lf = (int)(intptr_t)arg;
  struct seccomp_notif *req = calloc(1, sizeof *req + 4096);
  struct seccomp_notif_resp *resp = calloc(1, sizeof *resp + 4096);
  if (!req || !resp) return NULL;
  for (;;) {
    memset(req, 0, sizeof *req);
    if (ioctl(lf, SECCOMP_IOCTL_NOTIF_RECV, req) != 0) {
      if (errno == EINTR) continue;
      break; // listener gone: tracee tree exited
    }
    handle_openat(lf, req, resp);
  }
  free(req);
  free(resp);
  return NULL;
}

// ---- supervisor + tracer ---------------------------------------------------

int main(int argc, char **argv) {
  if (argc < 2) { fprintf(stderr, "usage: rcc_seccomp <target> [args...]\n"); return 2; }

  int sk[2];
  if (socketpair(AF_UNIX, SOCK_STREAM, 0, sk) != 0) { perror("socketpair"); return 1; }

  pid_t pid = fork();
  if (pid == 0) {
    close(sk[0]);
    // Become traceable by the supervisor, install the filter, hand the openat
    // listener fd back, then stop so the supervisor can set ptrace options
    // before our execvp (which will trap as PTRACE_EVENT_SECCOMP and route).
    ptrace(PTRACE_TRACEME, 0, 0, 0);
    int lf = install_filter();
    if (lf < 0) { perror("seccomp install"); _exit(97); }
    send_fd(sk[1], lf);
    close(lf); close(sk[1]);
    kill(getpid(), SIGSTOP);
    execvp(argv[1], &argv[1]);
    perror("exec");
    _exit(96);
  }
  close(sk[1]);
  int lf = recv_fd(sk[0]);
  if (lf < 0) { fprintf(stderr, "recv listener failed\n"); return 1; }

  // Wait for the child's post-TRACEME SIGSTOP, then arm ptrace: trap seccomp
  // RET_TRACE events (execve) and auto-attach fork/vfork/clone children so
  // subprocesses of subprocesses are traced too. EXITKILL tears the tree down
  // if the supervisor dies.
  int st;
  if (waitpid(pid, &st, 0) != pid) { perror("waitpid initial"); return 1; }
  long opts = PTRACE_O_TRACESECCOMP | PTRACE_O_TRACEFORK | PTRACE_O_TRACEVFORK |
              PTRACE_O_TRACECLONE | PTRACE_O_EXITKILL;
  if (ptrace(PTRACE_SETOPTIONS, pid, 0, (void *)opts) != 0) perror("setoptions");

  // Hand the openat listener to a dedicated thread; the main thread stays the
  // tracer (ptrace is thread-bound to the fork()ing thread).
  pthread_t th;
  pthread_create(&th, NULL, openat_thread, (void *)(intptr_t)lf);

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
        ptrace(PTRACE_CONT, w, sig, 0); // signal-delivery-stop: reinject
      }
    }
  }

  close(lf); // unblocks the openat thread's NOTIF_RECV
  return exit_code;
}
