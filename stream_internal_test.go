/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// White-box tests for stream.go that require access to unexported symbols
// (newStream, addSubscriber, deregister, Subscriber.connection,
// getSubscriberCount). Named *_internal_test.go per the testpackage linter
// convention so they are allowed to stay in package sse.
package sse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests are accessing subscriber in a non-threadsafe way.
// Maybe fix this in the future so we can test with -race enabled

func TestStreamAddSubscriber(t *testing.T) {
	// AutoReplay=false keeps the test simple: no event log replay, so exactly
	// one event arrives per publish. The previous version used AutoReplay=true
	// and sent an event before the subscriber registered; depending on goroutine
	// scheduling the event could be replayed at subscribe time AND dispatched
	// again, leaving two messages in the channel and breaking the len assertion.
	s := newStream("test", 1024, false, false, nil, nil)
	s.run()
	defer s.close()

	sub, ok := s.addSubscriber(0, nil)
	require.True(t, ok, "addSubscriber should succeed when MaxSubscribers is 0")
	assert.Equal(t, 1, s.getSubscriberCount())

	s.event <- &Event{Data: []byte("test")}
	msg, err := wait(sub.connection, time.Second*1)

	require.Nil(t, err)
	assert.Equal(t, []byte(`test`), msg)
	assert.Equal(t, 0, len(sub.connection))
}

func TestStreamRemoveSubscriber(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	s.run()
	defer s.close()

	sub, ok := s.addSubscriber(0, nil)
	require.True(t, ok)
	time.Sleep(time.Millisecond * 100)
	s.deregister <- sub
	time.Sleep(time.Millisecond * 100)

	assert.Equal(t, 0, s.getSubscriberCount())
}

func TestStreamSubscriberClose(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	s.run()
	defer s.close()

	sub, ok := s.addSubscriber(0, nil)
	require.True(t, ok)
	sub.close()
	time.Sleep(time.Millisecond * 100)

	assert.Equal(t, 0, s.getSubscriberCount())
}

func TestStreamDisableAutoReplay(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	s.run()
	defer s.close()

	s.AutoReplay = false
	s.event <- &Event{Data: []byte("test")}
	time.Sleep(time.Millisecond * 100)
	sub, ok := s.addSubscriber(0, nil)
	require.True(t, ok)

	assert.Equal(t, 0, len(sub.connection))
}

func TestStreamSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	s := newStream("test", 1024, false, false, nil, nil)
	s.run()
	defer s.close()

	slowSub, ok1 := s.addSubscriber(0, nil)
	require.True(t, ok1)
	fastSub, ok2 := s.addSubscriber(0, nil)
	require.True(t, ok2)

	// Wait for both subscribers to be registered
	time.Sleep(time.Millisecond * 100)

	// Fill the slow subscriber's channel to capacity so sends to it would block
	for i := 0; i < cap(slowSub.connection); i++ {
		slowSub.connection <- &Event{Data: []byte("filler")}
	}

	// Now publish an event — this must not block on the slow subscriber
	s.event <- &Event{Data: []byte("important")}

	// The fast subscriber should receive the event promptly
	select {
	case ev := <-fastSub.connection:
		assert.Equal(t, []byte("important"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("fast subscriber blocked by slow subscriber — dispatch is not non-blocking")
	}
}

func TestStreamMultipleSubscribers(t *testing.T) {
	var subs []*Subscriber

	s := newStream("test", 1024, true, false, nil, nil)
	s.run()

	for i := 0; i < 10; i++ {
		sub, ok := s.addSubscriber(0, nil)
		require.True(t, ok)
		subs = append(subs, sub)
	}

	// Wait for all subscribers to be added
	time.Sleep(time.Millisecond * 100)

	s.event <- &Event{Data: []byte("test")}
	for _, sub := range subs {
		msg, err := wait(sub.connection, time.Second*1)
		require.Nil(t, err)
		assert.Equal(t, []byte(`test`), msg)
	}

	s.close()

	// Wait for all subscribers to close
	time.Sleep(time.Millisecond * 100)
	assert.Equal(t, 0, s.getSubscriberCount())
}

// TestRemoveAllSubscribersWithRemovedChannel verifies sse-6t3: the
// removed != nil branch in removeAllSubscribers is exercised when a
// subscriber carries a removed channel (auto-stream path).
//
// We do NOT call s.run() because removeAllSubscribers is an internal method
// that operates on str.subscribers directly. Running the goroutine concurrently
// would race on that slice. Instead we construct the subscriber by hand
// (mirroring what addSubscriber does for isAutoStream==true) and inject it
// directly into str.subscribers, then call removeAllSubscribers from the same
// goroutine — no concurrent access, race-detector-clean.
func TestRemoveAllSubscribersWithRemovedChannel(t *testing.T) {
	s := newStream("test", 1024, false, true, nil, nil)
	// Do NOT call s.run() — we invoke removeAllSubscribers directly.

	// Build a subscriber the same way addSubscriber does for an auto-stream.
	removedCh := make(chan struct{}, 1)
	sub := &Subscriber{
		quit:       s.deregister,
		streamQuit: s.quit,
		connection: make(chan *Event, 64),
		removed:    removedCh,
	}

	// Inject directly so there is no concurrent goroutine touching the slice.
	s.subscribers = append(s.subscribers, sub)
	s.subscriberCount = 1

	// Call the function under test.
	s.removeAllSubscribers()

	// The removed channel (buffered 1) must have received the signal before
	// being closed.
	select {
	case _, ok := <-removedCh:
		assert.True(t, ok, "removed channel should deliver a struct{}{} before being closed")
	default:
		t.Fatal("removed channel should have received a signal but had none")
	}

	assert.Equal(t, 0, s.getSubscriberCount())
	assert.Empty(t, s.subscribers)
}

// TestStreamMaxSubscribersRejected verifies sse-6rz at the stream level:
// addSubscriber returns (nil, false) when MaxSubscribers is exceeded.
func TestStreamMaxSubscribersRejected(t *testing.T) {
	s := newStream("test", 1024, false, false, nil, nil)
	s.MaxSubscribers = 2
	s.run()
	defer s.close()

	sub1, ok1 := s.addSubscriber(0, nil)
	require.True(t, ok1, "first subscriber should be accepted")
	require.NotNil(t, sub1)

	sub2, ok2 := s.addSubscriber(0, nil)
	require.True(t, ok2, "second subscriber should be accepted (at cap)")
	require.NotNil(t, sub2)

	// Third subscriber should be rejected.
	sub3, ok3 := s.addSubscriber(0, nil)
	assert.False(t, ok3, "third subscriber must be rejected when at MaxSubscribers=2")
	assert.Nil(t, sub3)
}

// TestReplayDoesNotBlockOnSlowSubscriber verifies sse-xua:
// EventLog.Replay() previously did a direct blocking send into the
// subscriber's connection channel. If the channel was full, Replay would
// stall indefinitely inside the stream's run() goroutine, preventing all
// other events from being dispatched.
//
// The fix: Replay uses a non-blocking select so slow subscribers are skipped
// rather than blocking the goroutine.
//
// We do NOT call s.run() so we can call Replay directly from this goroutine
// without any concurrent draining.
func TestReplayDoesNotBlockOnSlowSubscriber(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	// Do NOT call s.run() — we invoke Replay directly.

	// Add an event to the log manually.
	s.Eventlog.Add(&Event{Data: []byte("replayed")})

	// Build a subscriber whose connection channel is already full.
	sub := &Subscriber{
		quit:       s.deregister,
		streamQuit: s.quit,
		connection: make(chan *Event, 1),
	}
	// Fill the connection channel to capacity so a blocking send would stall.
	sub.connection <- &Event{Data: []byte("filler")}

	// Replay must return promptly — not block on the full channel.
	done := make(chan struct{})
	go func() {
		s.Eventlog.Replay(sub)
		close(done)
	}()

	select {
	case <-done:
		// Good — Replay returned without blocking.
	case <-time.After(2 * time.Second):
		t.Fatal("Replay blocked on a full subscriber channel — sse-xua not fixed")
	}
}
