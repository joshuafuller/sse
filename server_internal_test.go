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
	"encoding/base64"
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

// TestServerProcessEncodeBase64False verifies that server.process with
// EncodeBase64=false (the default) returns the event data unchanged.
func TestServerProcessEncodeBase64False(t *testing.T) {
	s := New()
	defer s.Close()

	input := []byte("hello world")
	event := &Event{Data: append([]byte(nil), input...)}
	result := s.process(event)
	assert.Equal(t, input, result.Data,
		"process with EncodeBase64=false must leave data unchanged; got %q", result.Data)
}

// TestServerProcessEncodeBase64True verifies that server.process with
// EncodeBase64=true returns the event data base64-encoded.
func TestServerProcessEncodeBase64True(t *testing.T) {
	s := New()
	defer s.Close()
	s.EncodeBase64 = true

	input := []byte("hello world")
	event := &Event{Data: append([]byte(nil), input...)}
	result := s.process(event)

	// Compute the expected base64 encoding.
	expected := make([]byte, base64.StdEncoding.EncodedLen(len(input)))
	base64.StdEncoding.Encode(expected, input)

	assert.Equal(t, expected, result.Data,
		"process with EncodeBase64=true must base64-encode data; got %q, want %q",
		result.Data, expected)
}

// --- sse-e9d: TryPublish success and drop paths ---

// TestTryPublishSuccess verifies that TryPublish returns true and the event is
// delivered when the stream's event channel has capacity.
func TestTryPublishSuccess(t *testing.T) {
	s := New()
	defer s.Close()

	s.CreateStream("test")
	stream := s.getStream("test")
	sub, ok := stream.addSubscriber(0, nil)
	require.True(t, ok)

	got := s.TryPublish("test", &Event{Data: []byte("hello")})
	require.True(t, got, "TryPublish should return true when channel has capacity")

	msg, err := wait(sub.connection, time.Second)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), msg)
}

// TestTryPublishDrop verifies that TryPublish returns false and drops the event
// when the stream's event channel is full (no blocking).
func TestTryPublishDrop(t *testing.T) {
	s := New()
	s.BufferSize = 1 // tiny buffer — easy to fill
	defer s.Close()

	s.CreateStream("test")

	// Fill the event channel completely so the next send would block.
	stream := s.getStream("test")
	stream.event <- &Event{Data: []byte("fill")}

	// The channel is now full; TryPublish must drop and return false.
	got := s.TryPublish("test", &Event{Data: []byte("dropped")})
	assert.False(t, got, "TryPublish should return false when channel is full")

	// Drain the channel to confirm only the fill event is present (the
	// dropped event must not have been queued).
	select {
	case ev := <-stream.event:
		assert.Equal(t, []byte("fill"), ev.Data, "only the fill event should be in the channel")
	case <-time.After(time.Second):
		t.Fatal("expected fill event in channel, got timeout")
	}

	// No further event should be in the channel.
	select {
	case ev := <-stream.event:
		t.Fatalf("unexpected event in channel after drop: %q", ev.Data)
	default:
		// good — channel is empty
	}
}

// TestTryPublishNonExistentStream verifies that TryPublish returns false when
// the target stream does not exist.
func TestTryPublishNonExistentStream(t *testing.T) {
	s := New()
	defer s.Close()

	got := s.TryPublish("no-such-stream", &Event{Data: []byte("x")})
	assert.False(t, got, "TryPublish should return false for a non-existent stream")
}

// --- sse-tem: StreamExists ---

// TestStreamExistsTrue verifies that StreamExists returns true for a stream
// that has been created.
func TestStreamExistsTrue(t *testing.T) {
	s := New()
	defer s.Close()

	s.CreateStream("present")
	assert.True(t, s.StreamExists("present"), "StreamExists should return true for an existing stream")
}

// TestStreamExistsFalse verifies that StreamExists returns false for a stream
// that has not been created.
func TestStreamExistsFalse(t *testing.T) {
	s := New()
	defer s.Close()

	assert.False(t, s.StreamExists("absent"), "StreamExists should return false for a non-existent stream")
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
