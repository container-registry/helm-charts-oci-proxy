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
	started, release, shuttingDown := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})}
	s.RegisterOnShutdown(func() { close(shuttingDown) })
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
	// Shutdown runs its hooks after closing the listener, so the request is
	// still in flight once Shutdown is under way.
	<-shuttingDown
	// runServer must wait for the drain; returning now would abandon the request.
	select {
	case err := <-runErr:
		t.Fatalf("runServer returned while a request was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
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

// A request that outlives SHUTDOWN_TIMEOUT is cut off, and the server still
// stops cleanly instead of failing the process.
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
	clientErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		clientErr <- err
	}()
	<-started
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("runServer after the drain timeout: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer ignored the shutdown timeout")
	}
	select {
	case err := <-clientErr:
		if err == nil {
			t.Error("the blocked request got a response; its connection was not closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked request was not cut off")
	}
}
