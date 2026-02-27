/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// White-box tests for server.go that require access to unexported symbols
// (Server.getStream, Stream.addSubscriber, Subscriber.connection). Also
// defines the wait helper used by stream_internal_test.go.
// Named *_internal_test.go per the testpackage linter convention so they
// are allowed to stay in package sse.
package sse

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wait(ch chan *Event, duration time.Duration) ([]byte, error) {
	var err error
	var msg []byte

	select {
	case event := <-ch:
		msg = event.Data
	case <-time.After(duration):
		err = errors.New("timeout")
	}
	return msg, err
}

func TestServerCreateStream(t *testing.T) {
	s := New()
	defer s.Close()

	s.CreateStream("test")

	assert.NotNil(t, s.getStream("test"))
}

func TestServerWithCallback(t *testing.T) {
	funcA := func(string, *Subscriber) {}
	funcB := func(string, *Subscriber) {}

	s := NewWithCallback(funcA, funcB)
	defer s.Close()
	assert.NotNil(t, s.OnSubscribe)
	assert.NotNil(t, s.OnUnsubscribe)
}

func TestServerCreateExistingStream(t *testing.T) {
	s := New()
	defer s.Close()

	s.CreateStream("test")

	numGoRoutines := runtime.NumGoroutine()

	s.CreateStream("test")

	assert.NotNil(t, s.getStream("test"))
	assert.Equal(t, numGoRoutines, runtime.NumGoroutine())
}

func TestServerRemoveStream(t *testing.T) {
	s := New()
	defer s.Close()

	s.CreateStream("test")
	s.RemoveStream("test")

	assert.Nil(t, s.getStream("test"))
}

func TestServerRemoveNonExistentStream(t *testing.T) {
	s := New()
	defer s.Close()

	s.RemoveStream("test")

	assert.NotPanics(t, func() { s.RemoveStream("test") })
}

func TestServerExistingStreamPublish(t *testing.T) {
	s := New()
	defer s.Close()

	s.CreateStream("test")
	stream := s.getStream("test")
	sub, ok := stream.addSubscriber(0, nil)
	require.True(t, ok)

	s.Publish("test", &Event{Data: []byte("test")})

	msg, err := wait(sub.connection, time.Second*1)
	require.Nil(t, err)
	assert.Equal(t, []byte(`test`), msg)
}

func TestServerNonExistentStreamPublish(t *testing.T) {
	s := New()
	defer s.Close()

	s.RemoveStream("test")

	assert.NotPanics(t, func() { s.Publish("test", &Event{Data: []byte("test")}) })
}

// TestServerCloseNoGoroutineLeak verifies NF-005:
// After Server.Close(), the stream.run() goroutine must exit and the goroutine
// count must return to the baseline within 1 second.
// This is a TDD failing-first test for sse-8er.
func TestServerCloseNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	s := New()
	s.CreateStream("leak-test")

	stream := s.getStream("leak-test")
	require.NotNil(t, stream)

	// Add 3 subscribers directly (bypassing HTTP so we control their lifecycle).
	sub1, ok1 := stream.addSubscriber(0, nil)
	require.True(t, ok1)
	sub2, ok2 := stream.addSubscriber(0, nil)
	require.True(t, ok2)
	sub3, ok3 := stream.addSubscriber(0, nil)
	require.True(t, ok3)

	// Confirm events reach all subscribers.
	s.Publish("leak-test", &Event{Data: []byte("hello")})
	for _, sub := range []*Subscriber{sub1, sub2, sub3} {
		msg, err := wait(sub.connection, time.Second)
		require.NoError(t, err, "subscriber should receive event before Close")
		assert.Equal(t, []byte("hello"), msg)
	}

	// Close the server — this must stop the run() goroutine.
	s.Close()

	// Give goroutines up to 1 second to exit.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := runtime.NumGoroutine()
	assert.LessOrEqual(t, got, baseline,
		"goroutine count should return to baseline after Server.Close(); baseline=%d got=%d",
		baseline, got)
}

// TestSubscriberDeregisterOnDisconnect verifies NF-008:
// When a subscriber's HTTP connection is cancelled (context done), the
// subscriber must be removed from the stream and the context-done goroutine
// must not be permanently leaked (i.e., it must not block forever on a send
// to the deregister channel after the stream's run() goroutine has exited).
//
// The scenario that caused the leak:
//  1. Server.Close() fires → run() exits → nobody reads from str.deregister.
//  2. Client context cancels → sub.close() tries to send to str.deregister.
//  3. The send blocks forever → goroutine leaked.
func TestSubscriberDeregisterOnDisconnect(t *testing.T) {
	s := New()
	stream := s.CreateStream("deregister-test")

	sub, ok := stream.addSubscriber(0, nil)
	require.True(t, ok)

	// Wait for the subscriber to appear in the stream.
	require.Eventually(t, func() bool {
		return stream.getSubscriberCount() == 1
	}, time.Second, 10*time.Millisecond, "subscriber did not register")

	// Snapshot goroutine count after setup.
	goroutinesBeforeClose := runtime.NumGoroutine()

	// Close the server — run() will exit and close sub.connection.
	s.Close()

	// Now simulate what the ServeHTTP context-done goroutine does: call sub.close().
	// After the fix this must return promptly (not block forever).
	done := make(chan struct{})
	go func() {
		sub.close()
		close(done)
	}()

	select {
	case <-done:
		// Good — sub.close() returned without blocking.
	case <-time.After(2 * time.Second):
		t.Fatal("sub.close() blocked forever after stream.run() exited — goroutine leak confirmed")
	}

	// Goroutine count should settle back to at most what it was before Close.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= goroutinesBeforeClose {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), goroutinesBeforeClose,
		"goroutine count should not exceed pre-close baseline")
}
