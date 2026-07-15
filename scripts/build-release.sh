#!/usr/bin/env bash
# build-release.sh — build release archives for the HOST OS (both arches).
#
#   scripts/build-release.sh [version]        # e.g. scripts/build-release.sh v0.1.0
#
# On macOS : builds a universal (arm64+x86_64) interpose dylib, embeds it, and
#            produces rca_<ver>_darwin_{arm64,amd64}.tar.gz.
# On Linux : builds a static rcc_seccomp per arch (aarch64 cross for arm64),
#            embeds the matching one, and produces
#            rca_<ver>_linux_{arm64,amd64}.tar.gz.
#
# Archives land in ./dist along with per-file sha256 in checksums-<os>.txt.
# The release workflow (.github/workflows/release.yml) runs this on each OS
# runner; run it locally to reproduce an artifact bit-for-bit-ish (modulo
# toolchain versions).
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

VERSION="${1:-dev-$(git rev-parse --short HEAD)}"
DIST="$REPO/dist"
EMBED="$REPO/cmd/rca/embedded"
LDFLAGS="-s -w -X main.version=$VERSION"
mkdir -p "$DIST"

# go:embed picks up EVERYTHING in embedded/ — drop stale artifacts (e.g. the
# other platform's, from a previous local build) so archives stay minimal.
rm -f "$EMBED/rcc_interpose.dylib" "$EMBED/rcc_seccomp" "$EMBED/rg"

# Archive names carry no version (rca_darwin_arm64.tar.gz) so install
# one-liners can use GitHub's releases/latest/download/ URLs; the version is
# stamped inside the binary (`rca version`) and on the release tag.
build_go() { # $1=goos $2=goarch
  local out="$DIST/rca"
  rm -f "$out"
  # Stage the static ripgrep matching this target so the executor can run a
  # cross-OS claude's rg spawns (see internal/executor/exec.go, Makefile `rg`).
  GOOS="$1" GOARCH="$2" make -C "$REPO" rg >/dev/null
  # -buildvcs=false: version comes from ldflags; VCS stamping would fail in
  # containers/worktrees where .git isn't fully visible.
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o "$out" ./cmd/rca
  local tarball="$DIST/rca_$1_$2.tar.gz"
  tar -C "$DIST" -czf "$tarball" rca
  rm -f "$out"
  echo "built $(basename "$tarball")"
}

case "$(uname -s)" in
Darwin)
  # One universal dylib serves both darwin builds.
  make -C native/macos clean >/dev/null
  make -C native/macos CFLAGS="-O2 -Wall -Wextra -fPIC -arch arm64 -arch x86_64" >/dev/null
  lipo -info native/macos/rcc_interpose.dylib
  cp native/macos/rcc_interpose.dylib "$EMBED/"
  build_go darwin arm64
  build_go darwin amd64
  (cd "$DIST" && shasum -a 256 rca_darwin_*.tar.gz > checksums-darwin.txt)
  ;;
Linux)
  # Static supervisor per arch so the artifact runs on any glibc/musl distro.
  make -C native/linux clean >/dev/null
  make -C native/linux CFLAGS="-O2 -Wall -Wextra -static" >/dev/null
  cp native/linux/rcc_seccomp "$EMBED/"
  build_go linux amd64

  make -C native/linux clean >/dev/null
  make -C native/linux CC=aarch64-linux-gnu-gcc CFLAGS="-O2 -Wall -Wextra -static" >/dev/null
  cp native/linux/rcc_seccomp "$EMBED/"
  build_go linux arm64
  (cd "$DIST" && sha256sum rca_linux_*.tar.gz > checksums-linux.txt)
  ;;
*)
  echo "unsupported host $(uname -s)" >&2
  exit 1
  ;;
esac

ls -la "$DIST"
