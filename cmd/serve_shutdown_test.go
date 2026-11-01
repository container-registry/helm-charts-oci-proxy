package cmd

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// A request that is running when shutdown starts must still complete.
func TestShutdownDrainsInFlightRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() {
		runErr <- runServer(ctx, s, func() error { return s.Serve(ln) }, 10*time.Second, log.New(os.Stdout, "test-", 0))
	}()

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()
	<-started
	cancel()
	// Give Shutdown time to start before the handler returns.
	time.Sleep(200 * time.Millisecond)
	close(release)

	if r := <-got; r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request: body %q, err %v", r.body, r.err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("runServer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not return after the drain")
	}
}

func TestShutdownGivesUpAfterTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	started := make(chan struct{})
	s := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-block
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() {
		runErr <- runServer(ctx, s, func() error { return s.Serve(ln) }, 100*time.Millisecond, log.New(os.Stdout, "test-", 0))
	}()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String()); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("runServer returned nil although a request was still running")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer ignored the shutdown timeout")
	}
}
