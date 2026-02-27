/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Regression smoke tests — each test targets a known upstream bug that was
// fixed in this fork.  They serve as guardrails against re-introduction.
// Run with:  go test -run "TestSmokeLastEventID|TestSmokeUnsubscribe|TestSmokeBackoff" -v -race ./...
package sse_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmokeLastEventIDOnReconnect proves that the client sends the
// Last-Event-ID header on the second connection after a disconnect.
func TestSmokeLastEventIDOnReconnect(t *testing.T) {
	var connNum atomic.Int32
	lastIDCh := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connNum.Add(1)
		if n == 1 {
			// First connection: send one event with id "42", then return
			// (closing the connection triggers EOF -> client reconnect).
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			f := w.(http.Flusher)
			f.Flush()
			fmt.Fprint(w, "id: 42\ndata: first\n\n")
			f.Flush()
			return // close connection -> EOF
		}
		// Second+ connection: capture Last-Event-ID, then 204 to stop reconnect.
		lastIDCh <- r.Header.Get("Last-Event-ID")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := sse.NewClient(srv.URL)
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	// SubscribeRawWithContext will return once it gets the 204.
	_ = c.SubscribeRawWithContext(context.Background(), func(e *sse.Event) {})

	select {
	case got := <-lastIDCh:
		assert.Equal(t, "42", got, "client must send Last-Event-ID: 42 on reconnect")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second connection with Last-Event-ID")
	}
}

// TestSmokeUnsubscribeWhileBackingOff is a regression test for sse-vuw:
// Unsubscribe must not deadlock when the client goroutine is sleeping in a
// backoff retry loop.
func TestSmokeUnsubscribeWhileBackingOff(t *testing.T) {
	// Server always returns 503 -> client retries indefinitely.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := sse.NewClient(srv.URL)
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *sse.Event, 64)
	go func() { _ = c.SubscribeChanWithContext(ctx, "test", ch) }()

	// Let the client enter the retry/backoff loop.
	time.Sleep(200 * time.Millisecond)

	// Unsubscribe must return promptly, not deadlock.
	done := make(chan struct{})
	go func() {
		c.Unsubscribe(ch)
		close(done)
	}()

	select {
	case <-done:
		// Good — Unsubscribe returned without deadlocking.
	case <-time.After(2 * time.Second):
		t.Fatal("Unsubscribe deadlocked while client was in backoff sleep")
	}
}

// TestSmokeBackoffExhaustion proves that when ReconnectStrategy's
// MaxElapsedTime expires, Subscribe returns a meaningful non-nil error
// (not nil, not context.Canceled).
func TestSmokeBackoffExhaustion(t *testing.T) {
	// Server always returns 503.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := sse.NewClient(srv.URL)
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 300 * time.Millisecond
	b.InitialInterval = 50 * time.Millisecond
	c.ReconnectStrategy = b

	err := c.SubscribeWithContext(context.Background(), "test", func(e *sse.Event) {})

	require.Error(t, err, "SubscribeWithContext must return non-nil error when backoff is exhausted")
	assert.False(t, errors.Is(err, context.Canceled),
		"error should not be context.Canceled; got: %v", err)
	t.Logf("backoff exhaustion error: %v", err)
}
