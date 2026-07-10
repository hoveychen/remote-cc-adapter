# Build the Linux seccomp + ptrace supervisor (subprocess routing).
#
# The adapter launches claude as: rcc_seccomp <claude> [args...]. See ../README.md.
# Requires Linux kernel headers (linux/seccomp.h, sys/ptrace.h) and glibc >= 2.27.

BIN    := rcc_seccomp
SRC    := rcc_seccomp.c
CFLAGS ?= -O2 -Wall -Wextra

.PHONY: all clean
all: $(BIN)

$(BIN): $(SRC)
	$(CC) $(CFLAGS) -o $@ $<

clean:
	rm -f $(BIN)
