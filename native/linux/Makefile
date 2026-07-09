# Build the Linux seccomp-user-notify + ptrace supervisor.
#
# The adapter launches claude as: rcc_seccomp <claude> [args...]. See ../README.md.
# Requires Linux kernel headers (linux/seccomp.h, sys/ptrace.h) and glibc >= 2.27.
# -pthread: the tracer runs on the main thread while the openat listener runs on
# a second thread (ptrace is bound to the fork()ing thread).

BIN    := rcc_seccomp
SRC    := rcc_seccomp.c
CFLAGS ?= -O2 -Wall -Wextra -pthread

.PHONY: all clean
all: $(BIN)

$(BIN): $(SRC)
	$(CC) $(CFLAGS) -o $@ $<

clean:
	rm -f $(BIN)
