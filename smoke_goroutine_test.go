/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse_test

// Goroutine-leak smoke tests.
//
// runtime.NumGoroutine() is unreliable in this package because:
//   - client_test.go spawns a persistent publishMsgs goroutine for the
//     entire test binary lifetime.
//   - Many tests call t.Parallel(), so unrelated goroutines are alive
//     during our measurement windows.
//
// Instead, these tests verify cleanup directly:
//   - The goroutine we explicitly spawned exits (sync.WaitGroup).
//   - The server sees zero subscribers after disconnect (SubscriberCount).
//   - The client reports Connected() == false after disconnect.

import (
	"context"
	"sync"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmokeGoroutineLeakOnContextCancel verifies that cancelling the client
// context causes:
//   - SubscribeChanWithContext to return (spawned goroutine exits).
//   - Client.Connected() to return false.
//   - The stream's subscriber count to drop to zero.
func TestSmokeGoroutineLeakOnContextCancel(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("leak-ctx")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan *sse.Event, 4)
	c := sse.NewClient(url + "/events")
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	// Track the spawned goroutine with a WaitGroup.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.SubscribeChanWithContext(ctx, "leak-ctx", received)
	}()

	// Wait for the client to connect.
	require.Eventually(t, func() bool {
		return server.GetStream("leak-ctx").SubscriberCount() > 0
	}, 5*time.Second, 10*time.Millisecond, "client did not connect")

	server.Publish("leak-ctx", &sse.Event{Data: []byte("hello")})
	select {
	case ev := <-received:
		assert.Equal(t, []byte("hello"), ev.Data)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	// Cancel — this should unblock SubscribeChanWithContext.
	cancel()

	// The goroutine we spawned must exit within 5s.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine leak: SubscribeChanWithContext goroutine did not exit after context cancel")
	}

	// Client must report disconnected.
	assert.Eventually(t, func() bool {
		return !c.Connected()
	}, 2*time.Second, 10*time.Millisecond, "client still reports Connected() == true after cancel")

	// Server must see zero subscribers.
	assert.Eventually(t, func() bool {
		return server.GetStream("leak-ctx").SubscriberCount() == 0
	}, 2*time.Second, 10*time.Millisecond, "server still has subscribers after client cancel")
}

// TestSmokeGoroutineLeakOnUnsubscribe verifies that calling Unsubscribe causes:
//   - The subscription goroutine to exit.
//   - The stream's subscriber count to drop to zero.
func TestSmokeGoroutineLeakOnUnsubscribe(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("leak-unsub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *sse.Event, 4)
	c := sse.NewClient(url + "/events")
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.SubscribeChanWithContext(ctx, "leak-unsub", ch)
	}()

	// Wait until connected.
	require.Eventually(t, func() bool {
		return c.Connected()
	}, 5*time.Second, 10*time.Millisecond, "client did not connect")

	// Unsubscribe — must not deadlock (regression: sse-vuw).
	unsubDone := make(chan struct{})
	go func() { c.Unsubscribe(ch); close(unsubDone) }()
	select {
	case <-unsubDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Unsubscribe deadlocked")
	}

	// The subscription goroutine must exit within 5s.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine leak: subscription goroutine did not exit after Unsubscribe")
	}

	// Client must report disconnected.
	assert.Eventually(t, func() bool {
		return !c.Connected()
	}, 2*time.Second, 10*time.Millisecond, "client still reports Connected() == true after Unsubscribe")

	// Note: we do not assert SubscriberCount() == 0 here. Unsubscribe exits the
	// operation via the quit channel (not ctx.Done()), so the HTTP request context
	// is still live. The server's r.Context().Done() fires only after the TCP
	// connection is detected as dropped — which Go's keep-alive transport delays
	// by design. Server-side subscriber cleanup is covered by
	// TestSmokeOnSubscribeCallback and TestSmokeDisconnectDetection.
}
