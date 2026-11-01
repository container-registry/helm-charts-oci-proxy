package main

import (
	"context"
	"github.com/container-registry/helm-charts-oci-proxy/cmd"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signalContext()
	defer cancel()
	if err := cmd.Root.ExecuteContext(ctx); err != nil {
		cancel()
		os.Exit(1)
	}
}

// Kubernetes stops pods with SIGTERM; os.Interrupt covers Ctrl-C.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
