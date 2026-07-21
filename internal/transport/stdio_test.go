package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestStdioTransportMultiStream drives the core assumption behind the stdio
// transport: multiple concurrent logical streams can be opened over ONE pipe
// (as the executor does — a StreamKindFS stream plus one StreamKindExec stream
// per subprocess). Server echoes each stream; client opens N in parallel and
// checks each round-trips independently.
func TestStdioTransportMultiStream(t *testing.T) {
	c1, c2 := net.Pipe()

	ln, err := NewStdioListener(c1, c1, c1)
	if err != nil {
		t.Fatalf("NewStdioListener: %v", err)
	}
	defer ln.Close()

	// Server: accept streams forever, echo each one back.
	go func() {
		for {
			s, err := ln.Accept()
			if err != nil {
				return
			}
			go func(s Stream) {
				defer s.Close()
				_, _ = io.Copy(s, s)
			}(s)
		}
	}()

	d, err := NewStdioDialer(c2, c2, c2)
	if err != nil {
		t.Fatalf("NewStdioDialer: %v", err)
	}
	defer d.Close()

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, err := d.Dial(ctx)
			if err != nil {
				errs <- fmt.Errorf("stream %d dial: %w", i, err)
				return
			}
			defer s.Close()
			want := fmt.Sprintf("payload-for-stream-%d", i)
			if _, err := io.WriteString(s, want); err != nil {
				errs <- fmt.Errorf("stream %d write: %w", i, err)
				return
			}
			// Half-close our write side so the echo's io.Copy sees EOF and
			// stops — proving yamux stream half-close works in-band (the exact
			// semantic ssh -L mangled for raw unix sockets).
			if cw, ok := s.(interface{ CloseWrite() error }); ok {
				if err := cw.CloseWrite(); err != nil {
					errs <- fmt.Errorf("stream %d closewrite: %w", i, err)
					return
				}
			}
			got, err := io.ReadAll(s)
			if err != nil {
				errs <- fmt.Errorf("stream %d read: %w", i, err)
				return
			}
			if string(got) != want {
				errs <- fmt.Errorf("stream %d: got %q want %q", i, got, want)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
