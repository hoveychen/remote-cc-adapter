// rcc_seccomp.c — Linux seccomp-user-notify interception for remote-cc-adapter.
//
// This is the brain-side interception載体 for Linux (design doc §4.1.3 / §4.2).
// Unlike macOS, Bun on Linux issues raw syscalls that bypass glibc, so
// LD_PRELOAD symbol interposition misses its file I/O; a seccomp filter traps at
// the syscall boundary instead and catches everything.
//
// Model (ported from the validated POC seccomp/sec.c):
//   - fork; the child installs a seccomp filter that turns openat into a
//     USER_NOTIF, hands the listener fd to the parent (supervisor) over a
//     socketpair via SCM_RIGHTS, then execs the target (claude).
//   - the supervisor loops on NOTIF_RECV, reads the path from /proc/<pid>/mem,
//     and for routed paths opens the file's FUSE-backed entry under RCC_FUSE_MNT
//     and injects that fd with NOTIF_ADDFD(FLAG_SEND).
//   - non-routed opens get FLAG_CONTINUE (the kernel runs the real openat).
//
// Lazy slicing (design doc §4.1.3 step 2 / §4.3): seccomp only traps openat, so
// the injected fd's later read/lseek are real syscalls we no longer see. Rather
// than fetch the whole file up front, the injected fd points at a FUSE file
// backed by `rca _fuse` (internal/linuxfuse), which serves each read as an on-demand slice from the
// adapter. Only files opened through the mount incur FUSE callbacks, so the
// target's other I/O is untouched.
//
// Invocation (by the adapter, see internal/adapter/launch.go):
//   rcc_seccomp <target-binary> [args...]
// Environment: RCC_FUSE_MNT (the rcc-fuse mount point), RCC_REMOTE_PREFIXES.

#define _GNU_SOURCE
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
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
#include <sys/mman.h>
#include <linux/seccomp.h>
#include <linux/filter.h>
#include <linux/audit.h>

#ifndef SECCOMP_ADDFD_FLAG_SEND
#define SECCOMP_ADDFD_FLAG_SEND (1UL << 1)
#endif
#ifndef SECCOMP_USER_NOTIF_FLAG_CONTINUE
#define SECCOMP_USER_NOTIF_FLAG_CONTINUE (1UL << 0)
#endif

enum { OP_OPEN = 2, OP_PREAD = 3, OP_CLOSE = 5 };

// ---- seccomp filter --------------------------------------------------------

static int seccomp(unsigned op, unsigned fl, void *a) { return syscall(__NR_seccomp, op, fl, a); }

static int install_filter(void) {
  struct sock_filter f[] = {
      BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
      BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_openat, 0, 1),
      BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_USER_NOTIF),
      BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
  };
  struct sock_fprog p = {.len = 4, .filter = f};
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

static char *read_path(pid_t pid, unsigned long addr) {
  static char b[4096];
  char mm[64];
  snprintf(mm, sizeof mm, "/proc/%d/mem", pid);
  int fd = open(mm, O_RDONLY);
  if (fd < 0) return NULL;
  ssize_t n = pread(fd, b, sizeof b - 1, addr);
  close(fd);
  if (n <= 0) return NULL;
  b[n] = 0;
  return b;
}

// ---- routing ---------------------------------------------------------------

static int is_remote(const char *path) {
  if (!path) return 0;
  const char *pre = getenv("RCC_REMOTE_PREFIXES");
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

// ---- FUSE-backed redirection -----------------------------------------------

// open_fuse opens the routed path's FUSE-backed file under RCC_FUSE_MNT and
// returns the fd (caller injects it). The FUSE daemon (`rca _fuse`) serves that
// file's reads by fetching slices from the adapter on demand, so nothing is
// materialised up front. The entry name is hex(path), matching
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

// ---- supervisor loop -------------------------------------------------------

int main(int argc, char **argv) {
  if (argc < 2) { fprintf(stderr, "usage: rcc_seccomp <target> [args...]\n"); return 2; }

  int sk[2];
  if (socketpair(AF_UNIX, SOCK_STREAM, 0, sk) != 0) { perror("socketpair"); return 1; }

  pid_t pid = fork();
  if (pid == 0) {
    close(sk[0]);
    int lf = install_filter();
    if (lf < 0) { perror("seccomp install"); _exit(97); }
    send_fd(sk[1], lf);
    close(lf); close(sk[1]);
    execvp(argv[1], &argv[1]);
    perror("exec");
    _exit(96);
  }
  close(sk[1]);
  int lf = recv_fd(sk[0]);
  if (lf < 0) { fprintf(stderr, "recv listener failed\n"); return 1; }

  struct seccomp_notif *req = calloc(1, sizeof *req + 4096);
  struct seccomp_notif_resp *resp = calloc(1, sizeof *resp + 4096);
  for (;;) {
    memset(req, 0, sizeof *req);
    if (ioctl(lf, SECCOMP_IOCTL_NOTIF_RECV, req)) { if (errno == EINTR) continue; break; }
    char *path = read_path(req->pid, req->data.args[1]);
    if (path && is_remote(path)) {
      int ffd = open_fuse(path);
      if (ffd >= 0) {
        struct seccomp_notif_addfd af = {0};
        af.id = req->id; af.flags = SECCOMP_ADDFD_FLAG_SEND; af.srcfd = ffd; af.newfd = 0;
        ioctl(lf, SECCOMP_IOCTL_NOTIF_ADDFD, &af);
        close(ffd);
        continue; // ADDFD_FLAG_SEND completes the notification; reads go via FUSE
      }
    }
    memset(resp, 0, sizeof *resp);
    resp->id = req->id;
    resp->flags = SECCOMP_USER_NOTIF_FLAG_CONTINUE;
    ioctl(lf, SECCOMP_IOCTL_NOTIF_SEND, resp);
  }

  int st;
  waitpid(pid, &st, 0);
  return WIFEXITED(st) ? WEXITSTATUS(st) : 1;
}
