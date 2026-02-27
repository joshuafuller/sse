/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Load and concurrency smoke tests — verify the server handles many
// subscribers and that slow subscribers do not block fast ones.
// Run with:  go test -run "TestSmokeConcurrent|TestSmokeSlowSubscriber" -v -race ./...
package sse_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmokeConcurrentSubscribers launches 50 concurrent subscribers on the
// same stream, publishes 5 events, and asserts every subscriber receives all 5.
func TestSmokeConcurrentSubscribers(t *testing.T) {
	url, server, teardown := smokeServer(t)

	server.CreateStream("load")

	const numSubs = 50
	const numEvents = 5

	ctxs := make([]context.Context, numSubs)
	cancels := make([]context.CancelFunc, numSubs)
	channels := make([]chan *sse.Event, numSubs)

	for i := 0; i < numSubs; i++ {
		ctxs[i], cancels[i] = context.WithCancel(context.Background())
		channels[i] = make(chan *sse.Event, numEvents+4)
	}
	// Cancel all client contexts before tearing down the server so httptest
	// does not block waiting for active connections.
	defer teardown()
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	// Launch all subscribers concurrently.
	var subWg sync.WaitGroup
	subWg.Add(numSubs)
	for i := 0; i < numSubs; i++ {
		go func(idx int) {
			defer subWg.Done()
			_ = smokeClient(url).SubscribeChanWithContext(ctxs[idx], "load", channels[idx])
		}(i)
	}

	// Wait until all 50 subscribers are registered on the stream.
	require.Eventually(t, func() bool {
		return server.GetStream("load").SubscriberCount() == numSubs
	}, 10*time.Second, 20*time.Millisecond, "not all subscribers connected")

	// Publish 5 events.
	for i := 0; i < numEvents; i++ {
		server.Publish("load", &sse.Event{Data: []byte(fmt.Sprintf("event-%d", i))})
	}

	// Each goroutine drains its channel expecting exactly 5 events.
	var doneWg sync.WaitGroup
	doneWg.Add(numSubs)
	var errCount atomic.Int32

	for i := 0; i < numSubs; i++ {
		go func(idx int) {
			defer doneWg.Done()
			received := 0
			deadline := time.After(15 * time.Second)
			for received < numEvents {
				select {
				case _, ok := <-channels[idx]:
					if !ok {
						errCount.Add(1)
						return
					}
					received++
				case <-deadline:
					errCount.Add(1)
					return
				}
			}
		}(i)
	}

	// Wait for all drain goroutines with a timeout.
	done := make(chan struct{})
	go func() {
		doneWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for all subscribers to receive events")
	}

	assert.Equal(t, int32(0), errCount.Load(),
		"some subscribers did not receive all %d events", numEvents)
}

// TestSmokeSlowSubscriberNoBlockOthers verifies that a slow subscriber does
// not delay event delivery to a fast subscriber. The server dispatcher uses a
// non-blocking send, so a full subscriber channel drops events for the slow
// subscriber without blocking the stream.
func TestSmokeSlowSubscriberNoBlockOthers(t *testing.T) {
	url, server, teardown := smokeServer(t)

	server.CreateStream("slow-test")

	const numEvents = 3

	// -- Fast subscriber (channel-based) --
	ctxFast, cancelFast := context.WithCancel(context.Background())
	fastCh := make(chan *sse.Event, numEvents+4)
	go func() {
		_ = smokeClient(url).SubscribeChanWithContext(ctxFast, "slow-test", fastCh)
	}()

	// -- Slow subscriber (handler-based, 500ms sleep per event) --
	ctxSlow, cancelSlow := context.WithCancel(context.Background())

	// Cancel clients before teardown so httptest.Server.Close does not block.
	defer teardown()
	defer cancelFast()
	defer cancelSlow()
	var slowReceived atomic.Int32
	go func() {
		_ = smokeClient(url).SubscribeWithContext(ctxSlow, "slow-test", func(msg *sse.Event) {
			slowReceived.Add(1)
			time.Sleep(500 * time.Millisecond)
		})
	}()

	// Wait until both subscribers are connected.
	require.Eventually(t, func() bool {
		return server.GetStream("slow-test").SubscriberCount() >= 2
	}, 5*time.Second, 20*time.Millisecond, "both subscribers did not connect")

	// Publish 3 events and record the time.
	start := time.Now()
	for i := 0; i < numEvents; i++ {
		server.Publish("slow-test", &sse.Event{Data: []byte(fmt.Sprintf("msg-%d", i))})
	}

	// The fast subscriber must receive all 3 events within 2 seconds.
	// If the slow subscriber were blocking the dispatcher, it would take
	// at least 3 * 500ms = 1.5s just to dispatch, plus network overhead.
	var fastGot int
	require.Eventually(t, func() bool {
		// Drain non-blocking.
		for {
			select {
			case _, ok := <-fastCh:
				if ok {
					fastGot++
				}
			default:
				return fastGot >= numEvents
			}
		}
	}, 2*time.Second, 10*time.Millisecond,
		"fast subscriber did not receive all %d events within 2s (got %d)", numEvents, fastGot)

	elapsed := time.Since(start)
	t.Logf("fast subscriber received %d events in %v", fastGot, elapsed)

	// Verify it was genuinely fast — well under the slow subscriber's total
	// processing time of 1.5s.
	assert.Less(t, elapsed, 1500*time.Millisecond,
		"fast subscriber was delayed by slow subscriber")
}
