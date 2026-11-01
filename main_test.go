package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestSignalContextCancelsOnSIGTERM(t *testing.T) {
	ctx, cancel := signalContext()
	defer cancel()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
}
