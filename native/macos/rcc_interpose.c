// rcc_interpose.c — macOS DYLD interpose layer for remote-cc-adapter.
//
// This is the brain-side interception載体 for macOS (design doc §4.1 / §4.2). It
// is loaded into a re-signed copy of the claude binary via
// DYLD_INSERT_LIBRARIES. It interposes libSystem's filesystem and subprocess
// entry points; for paths that match a remote prefix it forwards the operation
// to the adapter over a Unix socket using the binary IO-RPC protocol
// (internal/protocol), and for subprocesses it rewrites the spawn to launch the
// spawn proxy. Everything else falls through to the real syscall so the CLI
// boots, reads its own credentials, and writes ~/.claude locally.
//
// Ported from the validated POC dylibs (fakefd/fakevfs.c, robust/robust.c,
// spawn/spawnfwd.c, e2e/e2e.c). The material change from the POC is that the
// per-op data no longer comes from a local store on disk — it is fetched from
// the adapter via RPC, so the adapter's routing table (internal/routing) decides
// local-vs-remote and large files are read one slice at a time.
//
// Environment (set by the adapter, see internal/adapter/launch.go):
//   RCC_ADAPTER_SOCK    unix socket for fs IO-RPC
//   RCC_REMOTE_PREFIXES ':'-joined path prefixes to forward
//   RCC_SPAWN_PROXY     path to rcc-spawn-proxy
//   RCC_SPAWN_SENTINEL  env marker that forces a subprocess remote

#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdarg.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include <stdint.h>
#include <sys/stat.h>
#include <sys/param.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <dirent.h>
#include <spawn.h>
#include <pthread.h>

// ---- interpose plumbing ---------------------------------------------------

#define DI(_repl, _orig)                                                      \
  __attribute__((used)) static struct {                                       \
    const void *a;                                                            \
    const void *b;                                                            \
  } _ip_##_orig __attribute__((section("__DATA,__interpose"))) = {            \
      (const void *)&_repl, (const void *)&_orig};

extern int open_nc(const char *, int, ...) __asm("_open$NOCANCEL");
extern int openat_nc(int, const char *, int, ...) __asm("_openat$NOCANCEL");
extern ssize_t read_nc(int, void *, size_t) __asm("_read$NOCANCEL");
extern ssize_t pread_nc(int, void *, size_t, off_t) __asm("_pread$NOCANCEL");
extern ssize_t write_nc(int, const void *, size_t) __asm("_write$NOCANCEL");
extern int close_nc(int) __asm("_close$NOCANCEL");
extern int gde64(int, void *, size_t, long *) __asm("___getdirentries64");

// ---- protocol opcodes (mirror internal/protocol/protocol.go) --------------

enum { OP_STAT = 1, OP_OPEN = 2, OP_PREAD = 3, OP_WRITEFILE = 4, OP_CLOSE = 5, OP_READDIR = 6 };

// ---- audit log (opt-in via RCC_LOG=<path>) --------------------------------

static int LOGFD = -2; // -2 = unchecked, -1 = disabled
static pthread_mutex_t log_mtx = PTHREAD_MUTEX_INITIALIZER;
static void lg(const char *fmt, ...) {
  if (LOGFD == -1) return;
  if (LOGFD == -2) {
    const char *p = getenv("RCC_LOG");
    LOGFD = (p && *p) ? open(p, O_WRONLY | O_CREAT | O_APPEND, 0644) : -1;
    if (LOGFD < 0) { LOGFD = -1; return; }
  }
  char b[1024];
  va_list a;
  va_start(a, fmt);
  int n = vsnprintf(b, sizeof b, fmt, a);
  va_end(a);
  if (n > 0) {
    pthread_mutex_lock(&log_mtx);
    write(LOGFD, b, n);
    pthread_mutex_unlock(&log_mtx);
  }
}

// ---- RPC client (single serialized connection to the adapter) -------------

static pthread_mutex_t rpc_mtx = PTHREAD_MUTEX_INITIALIZER;
static int rpc_fd = -1;

static int rpc_connect(void) {
  if (rpc_fd >= 0) return 0;
  const char *path = getenv("RCC_ADAPTER_SOCK");
  if (!path || !*path) return -1;
  int fd = socket(AF_UNIX, SOCK_STREAM, 0);
  if (fd < 0) return -1;
  struct sockaddr_un sa;
  memset(&sa, 0, sizeof sa);
  sa.sun_family = AF_UNIX;
  strncpy(sa.sun_path, path, sizeof sa.sun_path - 1);
  if (connect(fd, (struct sockaddr *)&sa, sizeof sa) != 0) { close(fd); return -1; }
  rpc_fd = fd;
  return 0;
}

static int writen(int fd, const void *buf, size_t n) {
  const char *p = buf;
  while (n) {
    ssize_t w = write(fd, p, n);
    if (w <= 0) return -1;
    p += w; n -= (size_t)w;
  }
  return 0;
}
static int readn(int fd, void *buf, size_t n) {
  char *p = buf;
  while (n) {
    ssize_t r = read(fd, p, n);
    if (r <= 0) return -1;
    p += r; n -= (size_t)r;
  }
  return 0;
}

// A growable request/response buffer.
typedef struct { unsigned char *b; size_t len, cap; } buf_t;
static void bput(buf_t *s, const void *p, size_t n) {
  if (s->len + n > s->cap) { while (s->len + n > s->cap) s->cap = s->cap ? s->cap * 2 : 64; s->b = realloc(s->b, s->cap); }
  memcpy(s->b + s->len, p, n); s->len += n;
}
static void bu8(buf_t *s, uint8_t v) { bput(s, &v, 1); }
static void bu32(buf_t *s, uint32_t v) { unsigned char t[4] = {v >> 24, v >> 16, v >> 8, v}; bput(s, t, 4); }
static void bu64(buf_t *s, uint64_t v) {
  unsigned char t[8] = {v >> 56, v >> 48, v >> 40, v >> 32, v >> 24, v >> 16, v >> 8, v};
  bput(s, t, 8);
}
static void bstr(buf_t *s, const char *p) { size_t n = strlen(p); bu32(s, (uint32_t)n); bput(s, p, n); }

// Send a framed request body and read the framed response body. Returns a
// malloc'd response body (caller frees) and sets *rlen, or NULL on IO error.
static unsigned char *rpc_call(const buf_t *req, size_t *rlen) {
  pthread_mutex_lock(&rpc_mtx);
  if (rpc_connect() != 0) { pthread_mutex_unlock(&rpc_mtx); return NULL; }
  unsigned char hdr[4] = {req->len >> 24, req->len >> 16, req->len >> 8, req->len};
  if (writen(rpc_fd, hdr, 4) || writen(rpc_fd, req->b, req->len)) {
    close(rpc_fd); rpc_fd = -1; pthread_mutex_unlock(&rpc_mtx); return NULL;
  }
  if (readn(rpc_fd, hdr, 4)) { close(rpc_fd); rpc_fd = -1; pthread_mutex_unlock(&rpc_mtx); return NULL; }
  uint32_t n = (hdr[0] << 24) | (hdr[1] << 16) | (hdr[2] << 8) | hdr[3];
  unsigned char *body = malloc(n ? n : 1);
  if (n && readn(rpc_fd, body, n)) { free(body); close(rpc_fd); rpc_fd = -1; pthread_mutex_unlock(&rpc_mtx); return NULL; }
  pthread_mutex_unlock(&rpc_mtx);
  *rlen = n;
  return body;
}

// Response parser cursor.
typedef struct { const unsigned char *b; size_t off, len; int err; } cur_t;
static uint32_t cu32(cur_t *c) {
  if (c->err || c->off + 4 > c->len) { c->err = 1; return 0; }
  uint32_t v = (c->b[c->off] << 24) | (c->b[c->off + 1] << 16) | (c->b[c->off + 2] << 8) | c->b[c->off + 3];
  c->off += 4; return v;
}
static uint64_t cu64(cur_t *c) { uint64_t hi = cu32(c); uint64_t lo = cu32(c); return (hi << 32) | lo; }
static int32_t ci32(cur_t *c) { return (int32_t)cu32(c); }

// ---- routing --------------------------------------------------------------

// Returns 1 if path is under one of RCC_REMOTE_PREFIXES (forward to adapter).
// Mirrors the surgical remote-allowlist the end-to-end POC validated (§4.1.1).
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

// ---- virtual fd table -----------------------------------------------------

#define MAXF 256
static pthread_mutex_t tab_mtx = PTHREAD_MUTEX_INITIALIZER;
static struct {
  int fd;            // placeholder /dev/null fd, also the table key
  char *path;        // routed path
  uint64_t handle;   // adapter handle (read side)
  off_t off;         // read cursor
  long long size;    // known size
  int writable;      // buffering writes
  int isdir;         // directory listing
  unsigned char *wbuf; size_t wcap, wlen; // write buffer flushed on close
  char **names; unsigned char *dtypes; int ncount, npos; // dir entries + d_type
} tab[MAXF];

static int newslot(void) {
  int ph = open("/dev/null", O_RDONLY);
  if (ph < 0) return -1;
  pthread_mutex_lock(&tab_mtx);
  for (int i = 0; i < MAXF; i++)
    if (tab[i].fd == 0) { memset(&tab[i], 0, sizeof tab[i]); tab[i].fd = ph; pthread_mutex_unlock(&tab_mtx); return i; }
  pthread_mutex_unlock(&tab_mtx);
  close(ph);
  return -1;
}
static int slot(int fd) { for (int i = 0; i < MAXF; i++) if (tab[i].fd == fd) return i; return -1; }

static void fillst(struct stat *s, long long sz, int dir) {
  memset(s, 0, sizeof *s);
  s->st_mode = dir ? (S_IFDIR | 0755) : (S_IFREG | 0644);
  s->st_size = sz;
  s->st_nlink = 1;
}

// ---- RPC wrappers ---------------------------------------------------------

// remote stat: fills *s. Returns 0 ok, -1 with errno set, or -2 if not routed.
static int rpc_stat(const char *path, struct stat *s) {
  if (!is_remote(path)) return -2;
  buf_t req = {0}; bu8(&req, OP_STAT); bstr(&req, path);
  size_t rl; unsigned char *rb = rpc_call(&req, &rl); free(req.b);
  if (!rb) { errno = EIO; return -1; }
  cur_t c = {rb, 0, rl, 0}; int32_t err = ci32(&c);
  if (err != 0) { free(rb); errno = -err; return -1; }
  uint32_t mode = cu32(&c); int64_t size = (int64_t)cu64(&c); free(rb);
  fillst(s, size, (mode & S_IFDIR) == S_IFDIR);
  return 0;
}

// remote open: registers a virtual fd. Returns fd, -1 (errno), or -2 (not routed).
static int rpc_open(const char *path, int flags) {
  if (!is_remote(path)) return -2;
  lg("OPEN\t%s\tflags=0x%x\n", path, flags);
  int wr = (flags & (O_WRONLY | O_RDWR)) || (flags & O_CREAT);

  // Probe with stat first to learn dir/size and existence.
  struct stat st; int have = (rpc_stat(path, &st) == 0);

  if (have && S_ISDIR(st.st_mode)) {
    // Directory: fetch entries now, synthesize on __getdirentries64.
    buf_t req = {0}; bu8(&req, OP_READDIR); bstr(&req, path);
    size_t rl; unsigned char *rb = rpc_call(&req, &rl); free(req.b);
    if (!rb) { errno = EIO; return -1; }
    cur_t c = {rb, 0, rl, 0}; int32_t err = ci32(&c);
    if (err != 0) { free(rb); errno = -err; return -1; }
    uint32_t n = cu32(&c);
    int i = newslot(); if (i < 0) { free(rb); errno = EMFILE; return -1; }
    tab[i].path = strdup(path); tab[i].isdir = 1;
    tab[i].names = calloc(n ? n : 1, sizeof(char *));
    tab[i].dtypes = calloc(n ? n : 1, 1);
    tab[i].ncount = (int)n;
    for (uint32_t k = 0; k < n && !c.err; k++) {
      uint32_t sl = cu32(&c);
      if (c.off + sl > c.len) { c.err = 1; break; }
      char *nm = malloc(sl + 1); memcpy(nm, c.b + c.off, sl); nm[sl] = 0; c.off += sl;
      tab[i].names[k] = nm;
      tab[i].dtypes[k] = (c.off < c.len) ? c.b[c.off] : 0; // per-entry d_type
      c.off += 1;
    }
    free(rb);
    return tab[i].fd;
  }

  if (wr) {
    int i = newslot(); if (i < 0) { errno = EMFILE; return -1; }
    tab[i].path = strdup(path); tab[i].writable = 1; tab[i].wcap = 4096; tab[i].wbuf = malloc(tab[i].wcap); tab[i].wlen = 0;
    // O_TRUNC or nonexistent: start empty. Otherwise seed with existing bytes.
    if (have && !(flags & O_TRUNC)) {
      // Read the whole file via slices to seed the buffer.
      off_t o = 0;
      while (o < st.st_size) {
        buf_t rq = {0}; bu8(&rq, OP_OPEN); bu32(&rq, O_RDONLY); bstr(&rq, path);
        size_t orl; unsigned char *orb = rpc_call(&rq, &orl); free(rq.b);
        if (!orb) break;
        cur_t oc = {orb, 0, orl, 0}; if (ci32(&oc) != 0) { free(orb); break; }
        cu32(&oc); cu64(&oc); uint64_t h = cu64(&oc); free(orb);
        // slurp
        for (;;) {
          buf_t pr = {0}; bu8(&pr, OP_PREAD); bu64(&pr, h); bu64(&pr, (uint64_t)o); bu32(&pr, 65536);
          size_t prl; unsigned char *prb = rpc_call(&pr, &prl); free(pr.b);
          if (!prb) break;
          cur_t pc = {prb, 0, prl, 0}; if (ci32(&pc) != 0) { free(prb); break; }
          uint32_t dl = cu32(&pc);
          if (dl == 0) { free(prb); break; }
          if (tab[i].wlen + dl > tab[i].wcap) { while (tab[i].wlen + dl > tab[i].wcap) tab[i].wcap *= 2; tab[i].wbuf = realloc(tab[i].wbuf, tab[i].wcap); }
          memcpy(tab[i].wbuf + tab[i].wlen, prb + pc.off, dl); tab[i].wlen += dl; o += dl;
          free(prb);
        }
        buf_t cq = {0}; bu8(&cq, OP_CLOSE); bu64(&cq, h); size_t crl; free(rpc_call(&cq, &crl)); free(cq.b);
        break;
      }
    }
    return tab[i].fd;
  }

  if (!have) { errno = ENOENT; return -1; }
  // Read-only: open a remote handle for slice reads.
  buf_t req = {0}; bu8(&req, OP_OPEN); bu32(&req, (uint32_t)O_RDONLY); bstr(&req, path);
  size_t rl; unsigned char *rb = rpc_call(&req, &rl); free(req.b);
  if (!rb) { errno = EIO; return -1; }
  cur_t c = {rb, 0, rl, 0}; int32_t err = ci32(&c);
  if (err != 0) { free(rb); errno = -err; return -1; }
  cu32(&c); int64_t size = (int64_t)cu64(&c); uint64_t handle = cu64(&c); free(rb);
  int i = newslot(); if (i < 0) { errno = EMFILE; return -1; }
  tab[i].path = strdup(path); tab[i].handle = handle; tab[i].size = size;
  return tab[i].fd;
}

// remote slice read into b. Returns bytes read or -1.
static ssize_t rpc_pread(int i, void *b, size_t n, off_t off) {
  if (off >= tab[i].size) return 0;
  size_t want = n;
  if (off + (off_t)want > tab[i].size) want = (size_t)(tab[i].size - off);
  buf_t req = {0}; bu8(&req, OP_PREAD); bu64(&req, tab[i].handle); bu64(&req, (uint64_t)off); bu32(&req, (uint32_t)want);
  size_t rl; unsigned char *rb = rpc_call(&req, &rl); free(req.b);
  if (!rb) { errno = EIO; return -1; }
  cur_t c = {rb, 0, rl, 0}; int32_t err = ci32(&c);
  if (err != 0) { free(rb); errno = -err; return -1; }
  uint32_t dl = cu32(&c);
  if (dl > want) dl = (uint32_t)want;
  memcpy(b, rb + c.off, dl); free(rb);
  return (ssize_t)dl;
}

static void flush_write(int i) {
  buf_t req = {0}; bu8(&req, OP_WRITEFILE); bstr(&req, tab[i].path); bu32(&req, (uint32_t)tab[i].wlen);
  bput(&req, tab[i].wbuf, tab[i].wlen);
  size_t rl; unsigned char *rb = rpc_call(&req, &rl); free(req.b); free(rb);
}

// ---- interposed fs entry points -------------------------------------------

int my_stat(const char *p, struct stat *s) { int r = rpc_stat(p, s); if (r != -2) return r; return stat(p, s); }
int my_lstat(const char *p, struct stat *s) { int r = rpc_stat(p, s); if (r != -2) return r; return lstat(p, s); }
int my_fstat(int fd, struct stat *s) {
  int i = slot(fd);
  if (i >= 0) { fillst(s, tab[i].writable ? (long long)tab[i].wlen : tab[i].size, tab[i].isdir); return 0; }
  return fstat(fd, s);
}

static int doopen(const char *p, int f) { return rpc_open(p, f); }

// resolve_at joins a relative openat() path against a virtual directory fd so
// that openat(dirfd, "note.txt") on a routed dir resolves to the routed file.
// Returns a malloc'd absolute path (caller frees), or NULL to use p as-is.
//
// Note: this deliberately does NOT resolve openat(AT_FDCWD, "rel") or plain
// open("rel") against getcwd(). Routing every relative open against the cwd
// destabilises claude's own boot (it opens many relative paths that must stay
// local), and in practice claude's Read/Write tools resolve paths to absolute
// before opening, so routing sees them anyway. Natural routing of bare relative
// opens is left as future work (design doc §4.3).
static char *resolve_at(int d, const char *p) {
  if (!p || p[0] == '/') return NULL; // absolute or null: no join needed
  int i = slot(d);
  if (i < 0 || !tab[i].path) return NULL; // dirfd is not one of ours
  char *full = malloc(strlen(tab[i].path) + 1 + strlen(p) + 1);
  sprintf(full, "%s/%s", tab[i].path, p);
  return full;
}

int my_open(const char *p, int f, ...) { int r = doopen(p, f); if (r != -2) return r; mode_t m = 0; if (f & O_CREAT) { va_list a; va_start(a, f); m = va_arg(a, int); va_end(a); } return open(p, f, m); }
int my_open_nc(const char *p, int f, ...) { int r = doopen(p, f); if (r != -2) return r; mode_t m = 0; if (f & O_CREAT) { va_list a; va_start(a, f); m = va_arg(a, int); va_end(a); } return open_nc(p, f, m); }
int my_openat(int d, const char *p, int f, ...) { char *j = resolve_at(d, p); int r = doopen(j ? j : p, f); free(j); if (r != -2) return r; mode_t m = 0; if (f & O_CREAT) { va_list a; va_start(a, f); m = va_arg(a, int); va_end(a); } return openat(d, p, f, m); }
int my_openat_nc(int d, const char *p, int f, ...) { char *j = resolve_at(d, p); int r = doopen(j ? j : p, f); free(j); if (r != -2) return r; mode_t m = 0; if (f & O_CREAT) { va_list a; va_start(a, f); m = va_arg(a, int); va_end(a); } return openat_nc(d, p, f, m); }

// fcntl: virtual fds are placeholder /dev/null descriptors, so F_GETPATH would
// leak "/dev/null" (design doc §4.3). Return the routed path instead; other
// commands pass through.
int my_fcntl(int fd, int cmd, ...) {
  va_list a; va_start(a, cmd); void *arg = va_arg(a, void *); va_end(a);
  int i = slot(fd);
  if (i >= 0 && cmd == F_GETPATH && tab[i].path && arg) {
    strlcpy((char *)arg, tab[i].path, MAXPATHLEN);
    return 0;
  }
  return fcntl(fd, cmd, arg);
}

ssize_t my_read(int fd, void *b, size_t n) {
  int i = slot(fd);
  if (i < 0) return read(fd, b, n);
  if (tab[i].writable || tab[i].isdir) return 0;
  ssize_t g = rpc_pread(i, b, n, tab[i].off);
  if (g > 0) tab[i].off += g;
  return g;
}
ssize_t my_read_nc(int fd, void *b, size_t n) { return my_read(fd, b, n); }
ssize_t my_pread(int fd, void *b, size_t n, off_t o) {
  int i = slot(fd);
  if (i < 0) return pread(fd, b, n, o);
  if (tab[i].writable) return 0;
  return rpc_pread(i, b, n, o);
}
ssize_t my_pread_nc(int fd, void *b, size_t n, off_t o) { return my_pread(fd, b, n, o); }

static void wgrow(int i, size_t need) {
  if (tab[i].wlen + need > tab[i].wcap) { while (tab[i].wlen + need > tab[i].wcap) tab[i].wcap *= 2; tab[i].wbuf = realloc(tab[i].wbuf, tab[i].wcap); }
}
ssize_t my_write(int fd, const void *b, size_t n) {
  int i = slot(fd);
  if (i < 0 || !tab[i].writable) return write(fd, b, n);
  wgrow(i, n); memcpy(tab[i].wbuf + tab[i].wlen, b, n); tab[i].wlen += n; return (ssize_t)n;
}
ssize_t my_write_nc(int fd, const void *b, size_t n) { return my_write(fd, b, n); }
ssize_t my_pwrite(int fd, const void *b, size_t n, off_t o) {
  int i = slot(fd);
  if (i < 0 || !tab[i].writable) return pwrite(fd, b, n, o);
  if ((size_t)o + n > tab[i].wcap) { while ((size_t)o + n > tab[i].wcap) tab[i].wcap *= 2; tab[i].wbuf = realloc(tab[i].wbuf, tab[i].wcap); }
  memcpy(tab[i].wbuf + o, b, n); if ((size_t)o + n > tab[i].wlen) tab[i].wlen = (size_t)o + n; return (ssize_t)n;
}

off_t my_lseek(int fd, off_t o, int wh) {
  int i = slot(fd);
  if (i < 0) return lseek(fd, o, wh);
  long long sz = tab[i].writable ? (long long)tab[i].wlen : tab[i].size;
  off_t no = tab[i].off;
  if (wh == SEEK_SET) no = o; else if (wh == SEEK_CUR) no += o; else no = sz + o;
  tab[i].off = no; return no;
}

static int do_close(int fd, int useNc) {
  int i = slot(fd);
  if (i < 0) return useNc ? close_nc(fd) : close(fd);
  if (tab[i].writable) flush_write(i);
  else if (!tab[i].isdir && tab[i].handle) {
    buf_t req = {0}; bu8(&req, OP_CLOSE); bu64(&req, tab[i].handle); size_t rl; free(rpc_call(&req, &rl)); free(req.b);
  }
  if (tab[i].writable) free(tab[i].wbuf);
  if (tab[i].isdir) { for (int j = 0; j < tab[i].ncount; j++) free(tab[i].names[j]); free(tab[i].names); free(tab[i].dtypes); }
  pthread_mutex_lock(&tab_mtx); free(tab[i].path); tab[i].fd = 0; pthread_mutex_unlock(&tab_mtx);
  return useNc ? close_nc(fd) : close(fd);
}
int my_close(int fd) { return do_close(fd, 0); }
int my_close_nc(int fd) { return do_close(fd, 1); }

// __getdirentries64: synthesize dirents from the cached names (macOS dirent64
// record layout: 8B ino + 8B seekoff + 2B reclen + 2B namlen + 1B type + name).
int my_gde64(int fd, void *buf, size_t nbytes, long *basep) {
  int i = slot(fd);
  if (i < 0 || !tab[i].isdir) return gde64(fd, buf, nbytes, basep);
  char *out = (char *)buf; size_t used = 0;
  while (tab[i].npos < tab[i].ncount) {
    const char *nm = tab[i].names[tab[i].npos]; size_t nl = strlen(nm);
    size_t hdr = 8 + 8 + 2 + 2 + 1;
    size_t reclen = (hdr + nl + 1 + 3) & ~((size_t)3);
    if (used + reclen > nbytes) break;
    char *rec = out + used;
    *(unsigned long long *)(rec) = tab[i].npos + 1;
    *(unsigned long long *)(rec + 8) = tab[i].npos + 1;
    *(unsigned short *)(rec + 16) = (unsigned short)reclen;
    *(unsigned short *)(rec + 18) = (unsigned short)nl;
    *(unsigned char *)(rec + 20) = tab[i].dtypes ? tab[i].dtypes[tab[i].npos] : DT_REG;
    memcpy(rec + 21, nm, nl); rec[21 + nl] = 0;
    used += reclen; tab[i].npos++;
  }
  if (basep) *basep = tab[i].npos;
  return (int)used;
}

// ---- subprocess forwarding -------------------------------------------------

// Binaries that must always run locally even when the working directory routes
// remote: keychain access (credentials live on the brain host), the spawn proxy
// itself (avoid an infinite loop), and claude re-spawning itself (subagents run
// in-process; a spawned claude helper still needs the brain-local adapter
// socket + credentials). Extend via RCC_LOCAL_BINS (':'-joined substrings).
static int spawn_is_local_bin(const char *path) {
  if (!path) return 0;
  static const char *defaults[] = {"/usr/bin/security", "rcc-spawn-proxy", NULL};
  for (int i = 0; defaults[i]; i++)
    if (strstr(path, defaults[i])) return 1;
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

static int cwd_is_remote(void) {
  char cwd[4096];
  if (getcwd(cwd, sizeof cwd)) return is_remote(cwd);
  return 0;
}

// posix_spawn: decide whether the subprocess runs on the remote executor. It is
// rewritten into an exec of the spawn proxy (which streams it remotely, design
// doc §4.1 pt4) when it routes remote. Routing (highest precedence first):
//   1. rg-mode self-invocation (claude re-exec'd as its embedded ripgrep, marked
//      by a ripgrep flag like --no-config) with a remote cwd -> REMOTE, over the
//      allowlist, so the recursive walk runs on the executor's real filesystem in
//      one pass rather than as a per-syscall fs-interpose metadata storm.
//   2. local-binary allowlist -> LOCAL (credentials/self-spawn must stay local)
//   3. RCC_SPAWN_SENTINEL present in argv -> REMOTE (explicit opt-in / tests)
//   4. target binary under a remote prefix -> REMOTE
//   5. working directory under a remote prefix -> REMOTE (natural: a subprocess
//      of a remote-routed project runs where the project lives)
//   6. otherwise -> LOCAL
//
// The proxy rewrite preserves the ORIGINAL argv[0]: claude enters ripgrep mode
// only when argv[0]'s basename is "rg", so the executor must exec the binary
// with that argv[0] intact (see cmd/rcc-spawn-proxy, internal/execproto).
int my_posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa,
                   const posix_spawnattr_t *at, char *const av[], char *const ev[]) {
  const char *proxy = getenv("RCC_SPAWN_PROXY");
  const char *sentinel = getenv("RCC_SPAWN_SENTINEL");
  int n = 0;
  while (av && av[n]) n++;

  // ripgrep-mode marker: --no-config is a ripgrep flag claude never uses in
  // agent mode. Only route it remote when the walk targets a remote project.
  int rgmode = 0;
  for (int j = 1; j < n; j++)
    if (strcmp(av[j], "--no-config") == 0) { rgmode = 1; break; }

  int route = 0;
  const char *reason = "default-local";
  if (spawn_is_local_bin(path) && !(rgmode && cwd_is_remote())) {
    route = 0;
    reason = "local-allowlist";
  } else {
    int sent = 0;
    for (int j = 0; j < n; j++)
      if (sentinel && *sentinel && strstr(av[j], sentinel)) { sent = 1; break; }
    if (rgmode && cwd_is_remote()) { route = 1; reason = "rg-mode"; }
    else if (sent) { route = 1; reason = "sentinel"; }
    else if (is_remote(path)) { route = 1; reason = "target-prefix"; }
    else if (cwd_is_remote()) { route = 1; reason = "cwd-prefix"; }
  }
  lg("SPAWN\ttarget=%s\troute=%d\treason=%s\targ0=%s\targ1=%s\n", path ? path : "?",
     route, reason, (n > 0 && av[0]) ? av[0] : "", (n > 1 && av[1]) ? av[1] : "");

  if (route && proxy && *proxy) {
    // proxy argv: [proxy, exec-path, argv0, argv1, ...] — exec-path is the real
    // binary; the rest is the child's full argv with argv[0] preserved.
    char **na = calloc(n + 3, sizeof(char *));
    int k = 0;
    na[k++] = (char *)proxy;
    na[k++] = (char *)path;
    for (int j = 0; j < n; j++) na[k++] = av[j];
    na[k] = NULL;
    int r = posix_spawn(pid, proxy, fa, at, na, ev);
    free(na);
    return r;
  }
  return posix_spawn(pid, path, fa, at, av, ev);
}

// ---- interpose bindings ----------------------------------------------------

DI(my_stat, stat) DI(my_lstat, lstat) DI(my_fstat, fstat)
DI(my_open, open) DI(my_open_nc, open_nc) DI(my_openat, openat) DI(my_openat_nc, openat_nc)
DI(my_read, read) DI(my_read_nc, read_nc) DI(my_pread, pread) DI(my_pread_nc, pread_nc)
DI(my_write, write) DI(my_write_nc, write_nc) DI(my_pwrite, pwrite)
DI(my_lseek, lseek) DI(my_close, close) DI(my_close_nc, close_nc) DI(my_gde64, gde64)
DI(my_fcntl, fcntl)
DI(my_posix_spawn, posix_spawn)
