# remote-adapter — top-level build.
#
# One Go binary: rca. The native interceptor is platform-specific (macOS
# interpose dylib / Linux seccomp supervisor); `make native` builds it and
# copies it into cmd/rca/embedded/ so `make go` embeds it in the binary.

BIN := bin
GO  := go

EMBED_DIR := cmd/rca/embedded

UNAME_S := $(shell uname -s)

# Bundled ripgrep: cross-OS claude runs its embedded ripgrep by re-execing its
# own (host-OS) binary with argv[0]=rg, which cannot run on a different-OS
# executor. `make rg` stages a static ripgrep matching the build's target
# GOOS/GOARCH into cmd/rca/embedded/rg; the executor extracts and runs it when a
# routed rg spawn can't be resolved locally (see internal/executor/exec.go).
RG_VERSION := 14.1.1

.PHONY: all go native rg test clean fmt vet macos linux

all: native rg
	$(MAKE) go

## Build rca into ./bin (embeds whatever is in cmd/rca/embedded/)
go:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/rca ./cmd/rca

## Build the native interceptor for the host platform and stage it for embedding
native:
ifeq ($(UNAME_S),Darwin)
	$(MAKE) macos
else ifeq ($(UNAME_S),Linux)
	$(MAKE) linux
else
	@echo "native interceptor unsupported on $(UNAME_S)"
endif

macos:
	$(MAKE) -C native/macos
	cp native/macos/rcc_interpose.dylib $(EMBED_DIR)/

linux:
	$(MAKE) -C native/linux
	cp native/linux/rcc_seccomp $(EMBED_DIR)/

## Download + checksum-verify a static ripgrep for the target GOOS/GOARCH and
## stage it at $(EMBED_DIR)/rg. Honours GOOS/GOARCH env for cross-compilation.
rg:
	@set -e; \
	os=$$($(GO) env GOOS); arch=$$($(GO) env GOARCH); \
	case "$$os/$$arch" in \
	  linux/amd64)  triple=x86_64-unknown-linux-musl;  sha=4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e;; \
	  linux/arm64)  triple=aarch64-unknown-linux-gnu;  sha=c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f;; \
	  darwin/amd64) triple=x86_64-apple-darwin;        sha=fc87e78f7cb3fea12d69072e7ef3b21509754717b746368fd40d88963630e2b3;; \
	  darwin/arm64) triple=aarch64-apple-darwin;       sha=24ad76777745fbff131c8fbc466742b011f925bfa4fffa2ded6def23b5b937be;; \
	  *) echo "make rg: no ripgrep asset for $$os/$$arch"; exit 1;; \
	esac; \
	asset=ripgrep-$(RG_VERSION)-$$triple.tar.gz; \
	url=https://github.com/BurntSushi/ripgrep/releases/download/$(RG_VERSION)/$$asset; \
	tmp=$$(mktemp -d); \
	echo "make rg: fetching $$asset"; \
	curl -fsSL -o $$tmp/$$asset $$url; \
	got=$$( (shasum -a 256 $$tmp/$$asset 2>/dev/null || sha256sum $$tmp/$$asset) | awk '{print $$1}'); \
	if [ "$$got" != "$$sha" ]; then echo "make rg: checksum mismatch for $$asset: got $$got want $$sha"; exit 1; fi; \
	tar -xzf $$tmp/$$asset -C $$tmp; \
	mkdir -p $(EMBED_DIR); \
	cp $$tmp/ripgrep-$(RG_VERSION)-$$triple/rg $(EMBED_DIR)/rg; \
	chmod +x $(EMBED_DIR)/rg; \
	rm -rf $$tmp; \
	echo "make rg: staged ripgrep $(RG_VERSION) for $$os/$$arch -> $(EMBED_DIR)/rg"

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BIN)
	rm -f $(EMBED_DIR)/rcc_interpose.dylib $(EMBED_DIR)/rcc_seccomp $(EMBED_DIR)/rg
	-$(MAKE) -C native/macos clean
	-$(MAKE) -C native/linux clean
