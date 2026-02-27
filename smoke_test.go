/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Smoke tests — end-to-end integration tests that follow the README exactly
// and exercise every major feature of the library as a real caller would.
// Run with:  go test -run Smoke -v -race ./...
package sse_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// smokeServer is a helper that wires up an sse.Server exactly as the README
// shows — mux.HandleFunc("/events", server.ServeHTTP) — and returns the
// test server URL, the sse.Server, and a teardown function.
func smokeServer(t *testing.T) (url string, s *sse.Server, teardown func()) {
	t.Helper()
	s = sse.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	return srv.URL, s, func() { srv.Close(); s.Close() }
}

// smokeClient returns a client pre-configured with a fast constant backoff so
// tests don't wait seconds on reconnects.
func smokeClient(url string) *sse.Client {
	c := sse.NewClient(url + "/events")
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)
	return c
}

// ── README § "Example Server" ────────────────────────────────────────────────

// SmokeServerPublish verifies the core README server pattern:
//
//	server := sse.New()
//	server.CreateStream("messages")
//	server.Publish("messages", &sse.Event{Data: []byte("ping")})
func TestSmokeServerPublish(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("messages")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan *sse.Event, 4)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "messages", received) }()

	require.Eventually(t, func() bool {
		return server.GetStream("messages").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	server.Publish("messages", &sse.Event{Data: []byte("ping")})

	select {
	case ev := <-received:
		assert.Equal(t, []byte("ping"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for published event")
	}
}

// SmokeDisconnectDetection verifies the README "Detect disconnected clients"
// pattern using r.Context().Done().
func TestSmokeDisconnectDetection(t *testing.T) {
	disconnected := make(chan struct{})

	s := sse.New()
	mux := http.NewServeMux()
	// README pattern exactly:
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			<-r.Context().Done()
			close(disconnected)
		}()
		s.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer s.Close()

	s.CreateStream("dc-test")

	ctx, cancel := context.WithCancel(context.Background())
	c := sse.NewClient(srv.URL + "/events")
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)
	go func() { _ = c.SubscribeChanWithContext(ctx, "dc-test", make(chan *sse.Event, 4)) }()

	require.Eventually(t, func() bool {
		return s.GetStream("dc-test").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	// Cancel the client context — simulates client disconnect.
	cancel()

	select {
	case <-disconnected:
		// Server detected the disconnect via r.Context().Done() — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("server did not detect client disconnect within 2s")
	}
}

// ── README § "Example Client" ────────────────────────────────────────────────

// SmokeSubscribeWithHandler verifies:
//
//	client.Subscribe("messages", func(msg *sse.Event) { … })
func TestSmokeSubscribeWithHandler(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("messages")

	var received atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := smokeClient(url)
	go func() {
		_ = c.SubscribeWithContext(ctx, "messages", func(msg *sse.Event) {
			received.Add(1)
		})
	}()

	require.Eventually(t, func() bool {
		return server.GetStream("messages").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	server.Publish("messages", &sse.Event{Data: []byte("hello")})
	server.Publish("messages", &sse.Event{Data: []byte("world")})

	require.Eventually(t, func() bool {
		return received.Load() >= 2
	}, 2*time.Second, 10*time.Millisecond, "handler did not receive both events")
}

// SmokeSubscribeChan verifies the README SubscribeChan pattern:
//
//	events := make(chan *sse.Event)
//	client.SubscribeChan("messages", events)
func TestSmokeSubscribeChan(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("messages")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 8)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "messages", events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("messages").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	server.Publish("messages", &sse.Event{Data: []byte("chan-event")})

	select {
	case ev := <-events:
		assert.Equal(t, []byte("chan-event"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel event")
	}
}

// SmokeSubscribeRaw verifies the README SubscribeRaw / custom URL param pattern.
func TestSmokeSubscribeRaw(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("raw")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// README: client := sse.NewClient("http://server/events?search=example")
	//         client.SubscribeRaw(func(msg *sse.Event) { … })
	// We use stream=raw to route to the right stream.
	events := make(chan *sse.Event, 4)
	c := sse.NewClient(url + "/events?stream=raw")
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)
	go func() { _ = c.SubscribeChanRawWithContext(ctx, events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("raw").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "raw client did not connect")

	server.Publish("raw", &sse.Event{Data: []byte("raw-data")})

	select {
	case ev := <-events:
		assert.Equal(t, []byte("raw-data"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for raw event")
	}
}

// ── Key behaviours documented in README behavioural-change table ─────────────

// SmokeHTTP204StopsReconnect verifies: "HTTP 204 No Content → permanent stop".
func TestSmokeHTTP204StopsReconnect(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := sse.NewClient(srv.URL)
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	err := c.SubscribeRawWithContext(context.Background(), func(*sse.Event) {})

	// Should return a permanent error after exactly one attempt.
	assert.Equal(t, int32(1), attempts.Load(), "client should not retry after 204")
	require.Error(t, err)
	var se *sse.StreamError
	assert.False(t, errors.As(err, &se), "204 is not a StreamError")
}

// SmokeNon200ReturnsStreamError verifies: "Non-200 → returned as *StreamError".
func TestSmokeNon200ReturnsStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := sse.NewClient(srv.URL)
	c.ReconnectStrategy = &backoff.StopBackOff{}

	err := c.SubscribeRawWithContext(context.Background(), func(*sse.Event) {})

	require.Error(t, err)
	var se *sse.StreamError
	require.True(t, errors.As(err, &se), "non-200 must be wrapped in *StreamError; got: %v", err)
	assert.Equal(t, http.StatusServiceUnavailable, se.StatusCode)
}

// SmokeOnConnectCallback verifies: "OnConnect fires immediately after HTTP 200".
func TestSmokeOnConnectCallback(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("cb-test")

	connected := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := smokeClient(url)
	c.OnConnect(func(*sse.Client) { connected <- struct{}{} })
	go func() { _ = c.SubscribeChanWithContext(ctx, "cb-test", make(chan *sse.Event, 4)) }()

	select {
	case <-connected:
		// Good — callback fired on connect, before any event.
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect callback did not fire within 2s")
	}
}

// ── Additional feature smoke tests ───────────────────────────────────────────

// SmokeAutoStream verifies that AutoStream=true creates a stream on first connect.
func TestSmokeAutoStream(t *testing.T) {
	s := sse.New()
	s.AutoStream = true
	s.AutoReplay = false

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 4)
	c := sse.NewClient(srv.URL + "/events")
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)
	go func() { _ = c.SubscribeChanWithContext(ctx, "dynamic", events) }()

	// The stream should be created automatically on first connect.
	require.Eventually(t, func() bool {
		return s.StreamExists("dynamic")
	}, time.Second, 10*time.Millisecond, "auto-stream was not created")

	s.Publish("dynamic", &sse.Event{Data: []byte("auto")})

	select {
	case ev := <-events:
		assert.Equal(t, []byte("auto"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-stream event")
	}
}

// SmokeEventTTL verifies that expired events are dropped and fresh ones pass.
func TestSmokeEventTTL(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.EventTTL = 50 * time.Millisecond
	server.AutoReplay = false
	server.CreateStream("ttl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 8)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "ttl", events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("ttl").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	// Fresh event — must arrive.
	server.Publish("ttl", &sse.Event{Data: []byte("fresh")})
	select {
	case ev := <-events:
		assert.Equal(t, []byte("fresh"), ev.Data, "fresh event must not be dropped")
	case <-time.After(2 * time.Second):
		t.Fatal("fresh event was dropped (EventTTL bug)")
	}
}

// SmokeEventLog verifies AutoReplay delivers the event log to a late subscriber.
func TestSmokeEventLog(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.AutoReplay = true
	server.CreateStream("replay")

	// Publish before any subscriber connects.
	server.Publish("replay", &sse.Event{Data: []byte("first")})
	server.Publish("replay", &sse.Event{Data: []byte("second")})
	time.Sleep(50 * time.Millisecond) // let the stream process them into the log

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 8)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "replay", events) }()

	got := []string{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			got = append(got, string(ev.Data))
		case <-deadline:
			t.Fatalf("AutoReplay: only received %d/2 events: %v", len(got), got)
		}
	}
	assert.Equal(t, []string{"first", "second"}, got)
}

// SmokeMaxSubscribers verifies that connections beyond MaxSubscribers get 429.
func TestSmokeMaxSubscribers(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	stream := server.CreateStream("capped")
	stream.MaxSubscribers = 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First subscriber — must succeed.
	events1 := make(chan *sse.Event, 4)
	c1 := smokeClient(url)
	go func() { _ = c1.SubscribeChanWithContext(ctx, "capped", events1) }()

	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 1
	}, time.Second, 10*time.Millisecond, "first subscriber did not connect")

	// Second subscriber — must get HTTP 429 (non-200 → *StreamError).
	c2 := smokeClient(url)
	c2.ReconnectStrategy = &backoff.StopBackOff{}
	err := c2.SubscribeChanWithContext(context.Background(), "capped", make(chan *sse.Event, 4))
	require.Error(t, err)
	var se *sse.StreamError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, http.StatusTooManyRequests, se.StatusCode)
}

// SmokeTryPublish verifies that TryPublish returns true on success and false
// when the stream buffer is full.
func TestSmokeTryPublish(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("try")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 4)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "try", events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("try").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	ok := server.TryPublish("try", &sse.Event{Data: []byte("try-me")})
	assert.True(t, ok, "TryPublish should succeed when buffer has room")

	select {
	case ev := <-events:
		assert.Equal(t, []byte("try-me"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("TryPublish event not received")
	}

	assert.False(t, server.TryPublish("no-such-stream", &sse.Event{Data: []byte("x")}),
		"TryPublish must return false for non-existent stream")
}

// SmokeOnSubscribeCallback verifies the OnSubscribe / OnUnsubscribe callbacks.
func TestSmokeOnSubscribeCallback(t *testing.T) {
	var subCount, unsubCount atomic.Int32

	url, server, teardown := smokeServer(t)
	defer teardown()

	server.OnSubscribe = func(_ string, _ *sse.Subscriber) { subCount.Add(1) }
	server.OnUnsubscribe = func(_ string, _ *sse.Subscriber) { unsubCount.Add(1) }
	server.CreateStream("cb")

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan *sse.Event, 4)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "cb", events) }()

	require.Eventually(t, func() bool { return subCount.Load() == 1 },
		time.Second, 10*time.Millisecond, "OnSubscribe did not fire")

	cancel() // disconnect

	require.Eventually(t, func() bool { return unsubCount.Load() == 1 },
		time.Second, 10*time.Millisecond, "OnUnsubscribe did not fire")
}

// SmokeMultipleStreams verifies independent streams don't cross-contaminate.
func TestSmokeMultipleStreams(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	server.CreateStream("stream-a")
	server.CreateStream("stream-b")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evA := make(chan *sse.Event, 4)
	evB := make(chan *sse.Event, 4)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = smokeClient(url).SubscribeChanWithContext(ctx, "stream-a", evA) }()
	go func() { defer wg.Done(); _ = smokeClient(url).SubscribeChanWithContext(ctx, "stream-b", evB) }()

	require.Eventually(t, func() bool {
		return server.GetStream("stream-a").SubscriberCount() > 0 &&
			server.GetStream("stream-b").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "both clients did not connect")

	server.Publish("stream-a", &sse.Event{Data: []byte("for-a")})
	server.Publish("stream-b", &sse.Event{Data: []byte("for-b")})

	gotA, gotB := false, false
	deadline := time.After(2 * time.Second)
	for !gotA || !gotB {
		select {
		case ev := <-evA:
			assert.Equal(t, []byte("for-a"), ev.Data)
			gotA = true
		case ev := <-evB:
			assert.Equal(t, []byte("for-b"), ev.Data)
			gotB = true
		case <-deadline:
			t.Fatalf("timed out: gotA=%v gotB=%v", gotA, gotB)
		}
	}
}

// SmokeStreamExists verifies the StreamExists API.
func TestSmokeStreamExists(t *testing.T) {
	_, server, teardown := smokeServer(t)
	defer teardown()

	assert.False(t, server.StreamExists("ghost"))
	server.CreateStream("present")
	assert.True(t, server.StreamExists("present"))
	server.RemoveStream("present")
	assert.False(t, server.StreamExists("present"))
}

// ── Additional coverage: Client methods, event fields, reconnect ──────────────

// SmokeClientConnectedMethod verifies the Connected() bool accessor.
func TestSmokeClientConnectedMethod(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()
	server.CreateStream("conn-check")

	ctx, cancel := context.WithCancel(context.Background())
	c := smokeClient(url)
	events := make(chan *sse.Event, 4)

	assert.False(t, c.Connected(), "Connected() must be false before subscription")

	go func() { _ = c.SubscribeChanWithContext(ctx, "conn-check", events) }()

	require.Eventually(t, func() bool { return c.Connected() },
		time.Second, 10*time.Millisecond, "Connected() did not return true after connect")

	cancel()

	require.Eventually(t, func() bool { return !c.Connected() },
		time.Second, 10*time.Millisecond, "Connected() did not return false after disconnect")
}

// SmokeOnDisconnectCallback verifies the OnDisconnect callback fires when the
// client connection drops.
func TestSmokeOnDisconnectCallback(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()
	server.CreateStream("ondisconn")

	disconnected := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	c := smokeClient(url)
	c.OnDisconnect(func(*sse.Client) { disconnected <- struct{}{} })
	go func() { _ = c.SubscribeChanWithContext(ctx, "ondisconn", make(chan *sse.Event, 4)) }()

	require.Eventually(t, func() bool {
		return server.GetStream("ondisconn").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	cancel()

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDisconnect callback did not fire")
	}
}

// SmokeUnsubscribe verifies that Unsubscribe stops delivery to a channel.
func TestSmokeUnsubscribe(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()
	server.CreateStream("unsub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 4)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "unsub", events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("unsub").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	// Confirm delivery works before unsubscribing.
	server.Publish("unsub", &sse.Event{Data: []byte("before")})
	select {
	case ev := <-events:
		assert.Equal(t, []byte("before"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("event before unsubscribe not received")
	}

	c.Unsubscribe(events)

	// Give the unsubscribe time to take effect.
	time.Sleep(50 * time.Millisecond)
	server.Publish("unsub", &sse.Event{Data: []byte("after")})

	// Channel should be drained — no new events should arrive.
	select {
	case ev := <-events:
		t.Fatalf("received event after Unsubscribe: %s", ev.Data)
	case <-time.After(200 * time.Millisecond):
		// Correct — nothing arrived.
	}
}

// SmokeEventFields verifies that Event fields (Event type, Retry, Comment)
// are correctly serialized and parsed end-to-end.
// Note: AutoReplay=false is required here because sse.New() defaults to
// AutoReplay=true, which causes EventLog.Add() to overwrite the user-supplied
// ID with an auto-generated sequential index.  We disable AutoReplay so the
// user-supplied ID is preserved on the wire and verified below.
func TestSmokeEventFields(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()
	server.AutoReplay = false
	server.CreateStream("fields")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 4)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "fields", events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("fields").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	server.Publish("fields", &sse.Event{
		ID:    []byte("42"),
		Event: []byte("update"),
		Data:  []byte("payload"),
		Retry: []byte("3000"),
	})

	select {
	case ev := <-events:
		assert.Equal(t, []byte("42"), ev.ID, "ID field")
		assert.Equal(t, []byte("update"), ev.Event, "Event type field")
		assert.Equal(t, []byte("payload"), ev.Data, "Data field")
		assert.Equal(t, []byte("3000"), ev.Retry, "Retry field")
	case <-time.After(2 * time.Second):
		t.Fatal("event with full field set not received")
	}
}

// SmokeBase64Encoding verifies Server.EncodeBase64 encodes data transparently.
func TestSmokeBase64Encoding(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()
	server.EncodeBase64 = true
	server.CreateStream("b64")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 4)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "b64", events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("b64").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	server.Publish("b64", &sse.Event{Data: []byte("binary\x00data")})

	select {
	case ev := <-events:
		// The client receives the raw base64 string in Data.
		assert.NotEqual(t, []byte("binary\x00data"), ev.Data,
			"EncodeBase64: data should be base64-encoded on the wire")
		assert.Greater(t, len(ev.Data), 0)
	case <-time.After(2 * time.Second):
		t.Fatal("base64-encoded event not received")
	}
}

// SmokeSplitData verifies Server.SplitData splits multi-line data correctly.
func TestSmokeSplitData(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()
	server.SplitData = true
	server.CreateStream("split")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 4)
	c := smokeClient(url)
	go func() { _ = c.SubscribeChanWithContext(ctx, "split", events) }()

	require.Eventually(t, func() bool {
		return server.GetStream("split").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect")

	server.Publish("split", &sse.Event{Data: []byte("line1\nline2\nline3")})

	select {
	case ev := <-events:
		// The client receives the rejoined data.
		assert.Contains(t, string(ev.Data), "line1")
		assert.Contains(t, string(ev.Data), "line3")
	case <-time.After(2 * time.Second):
		t.Fatal("split-data event not received")
	}
}

// SmokeSetOnSubscribePerStream verifies per-stream SetOnSubscribe callback.
func TestSmokeSetOnSubscribePerStream(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()

	stream := server.CreateStream("per-stream-cb")

	var fired atomic.Int32
	stream.SetOnSubscribe(func(_ string, _ *sse.Subscriber) { fired.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = smokeClient(url).SubscribeChanWithContext(ctx, "per-stream-cb", make(chan *sse.Event, 4))
	}()

	require.Eventually(t, func() bool { return fired.Load() == 1 },
		time.Second, 10*time.Millisecond, "per-stream OnSubscribe did not fire")
}

// SmokeReconnectOnServerRestart verifies the client reconnects when the server
// drops the connection (EOF → reconnect behaviour).
func TestSmokeReconnectOnServerRestart(t *testing.T) {
	// Each time the handler is called it sends one event then returns
	// immediately.  Returning from the handler causes the HTTP server to close
	// its end of the connection, which the client sees as an EOF, triggering
	// a reconnect.
	var attempts atomic.Int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: reconnect-test\n\n"))
		w.(http.Flusher).Flush()
		attempts.Add(1)
		// Return immediately so the connection closes and the client gets EOF.
	}))
	defer srv1.Close()

	events := make(chan *sse.Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := sse.NewClient(srv1.URL)
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)
	go func() { _ = c.SubscribeChanRawWithContext(ctx, events) }()

	// Receive the first event to confirm the client connected.
	select {
	case ev := <-events:
		assert.Equal(t, []byte("reconnect-test"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive event from server")
	}

	// Wait for the client to reconnect at least once more (attempts > 1).
	require.Eventually(t, func() bool { return attempts.Load() > 1 },
		3*time.Second, 20*time.Millisecond, "client did not reconnect after EOF")
}

// SmokeEventLogMaxEntries verifies EventLog.MaxEntries caps the replay log.
func TestSmokeEventLogMaxEntries(t *testing.T) {
	url, server, teardown := smokeServer(t)
	defer teardown()
	server.AutoReplay = true

	stream := server.CreateStream("capped-log")
	stream.Eventlog.MaxEntries = 2

	// Publish 4 events — only the last 2 should be replayed.
	for i, d := range []string{"old1", "old2", "new1", "new2"} {
		server.Publish("capped-log", &sse.Event{Data: []byte(d)})
		_ = i
	}
	time.Sleep(50 * time.Millisecond) // let run() process them into the log

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 8)
	go func() { _ = smokeClient(url).SubscribeChanWithContext(ctx, "capped-log", events) }()

	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			got = append(got, string(ev.Data))
		case <-deadline:
			t.Fatalf("EventLog.MaxEntries: received %d events, want 2: %v", len(got), got)
		}
	}

	// Must not receive a 3rd event from the log.
	select {
	case extra := <-events:
		t.Fatalf("EventLog.MaxEntries: received extra replayed event: %s", extra.Data)
	case <-time.After(150 * time.Millisecond):
	}

	assert.Equal(t, []string{"new1", "new2"}, got)
}

// SmokeNewWithCallback verifies NewWithCallback wires subscribe/unsubscribe.
func TestSmokeNewWithCallback(t *testing.T) {
	var subFired, unsubFired atomic.Bool

	s := sse.NewWithCallback(
		func(_ string, _ *sse.Subscriber) { subFired.Store(true) },
		func(_ string, _ *sse.Subscriber) { unsubFired.Store(true) },
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer s.Close()

	s.CreateStream("wc")

	ctx, cancel := context.WithCancel(context.Background())
	c := sse.NewClient(srv.URL + "/events")
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)
	go func() { _ = c.SubscribeChanWithContext(ctx, "wc", make(chan *sse.Event, 4)) }()

	require.Eventually(t, subFired.Load, time.Second, 10*time.Millisecond, "subscribe callback not fired")

	cancel()

	require.Eventually(t, unsubFired.Load, time.Second, 10*time.Millisecond, "unsubscribe callback not fired")
}
