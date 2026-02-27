/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireNoGoroutineLeak runs fn, then polls until the goroutine count returns
// to baseline (within 10s). A brief settle period before measuring the baseline
// lets goroutines from preceding tests (http_test.go, client_test.go) fully exit
// before we snapshot — preventing false-positive leak detection.
func requireNoGoroutineLeak(t *testing.T, fn func()) {
	t.Helper()
	// Let goroutines from previous tests wind down before snapshotting baseline.
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	fn()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Print all goroutine stacks to help diagnose the leak.
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Errorf("goroutine leak: started with %d, now %d\n\nGoroutine dump:\n%s",
		before, runtime.NumGoroutine(), buf[:n])
}

// TestSmokeGoroutineLeakOnContextCancel verifies that cancelling the client
// context causes all goroutines to wind down cleanly.
func TestSmokeGoroutineLeakOnContextCancel(t *testing.T) {
	requireNoGoroutineLeak(t, func() {
		url, server, teardown := smokeServer(t)

		server.CreateStream("leak-ctx")

		ctx, cancel := context.WithCancel(context.Background())

		received := make(chan *sse.Event, 4)
		c := sse.NewClient(url + "/events")
		c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

		done := make(chan error, 1)
		go func() {
			done <- c.SubscribeChanWithContext(ctx, "leak-ctx", received)
		}()

		// Wait for the client to connect and receive one event.
		require.Eventually(t, func() bool {
			return server.GetStream("leak-ctx").SubscriberCount() > 0
		}, time.Second, 10*time.Millisecond, "client did not connect")

		server.Publish("leak-ctx", &sse.Event{Data: []byte("hello")})

		select {
		case ev := <-received:
			assert.Equal(t, []byte("hello"), ev.Data)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for event")
		}

		// Cancel context to trigger client teardown.
		cancel()

		// Wait for SubscribeChanWithContext to return.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("SubscribeChanWithContext did not return after cancel")
		}

		// Close the client's idle HTTP connections so transport goroutines exit.
		c.Connection.CloseIdleConnections()

		// Shut down the server so its goroutines wind down too.
		teardown()
	})
}

// TestSmokeGoroutineLeakOnUnsubscribe verifies that calling Unsubscribe causes
// all goroutines to wind down cleanly.
func TestSmokeGoroutineLeakOnUnsubscribe(t *testing.T) {
	requireNoGoroutineLeak(t, func() {
		url, server, teardown := smokeServer(t)

		server.CreateStream("leak-unsub")

		ctx, cancel := context.WithCancel(context.Background())

		ch := make(chan *sse.Event, 4)
		c := sse.NewClient(url + "/events")
		c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

		go func() {
			_ = c.SubscribeChanWithContext(ctx, "leak-unsub", ch)
		}()

		// Wait until connected.
		require.Eventually(t, func() bool {
			return c.Connected()
		}, time.Second, 10*time.Millisecond, "client did not connect")

		// Unsubscribe to trigger client teardown.
		c.Unsubscribe(ch)

		// Close the client's idle HTTP connections so transport goroutines exit.
		c.Connection.CloseIdleConnections()

		// Cancel context and shut down server so all goroutines wind down.
		cancel()
		teardown()
	})
}
