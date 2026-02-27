/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	backoff "github.com/cenkalti/backoff/v4"
)

var urlPath string
var srv *Server
var server *httptest.Server

var mldata = `{
	"key": "value",
	"array": [
		1,
		2,
		3
	]
}`

func setup(empty bool) {
	// New Server
	srv = newServer()
	// Send almost-continuous string of events to the client
	go publishMsgs(srv, empty, 100000000)
}

func setupMultiline() {
	srv = newServer()
	srv.SplitData = true
	go publishMultilineMessages(srv, 100000000)
}

func setupCount(empty bool, count int) {
	srv = newServer()
	go publishMsgs(srv, empty, count)
}

func newServer() *Server {
	srv = New()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", srv.ServeHTTP)
	server = httptest.NewServer(mux)
	urlPath = server.URL + "/events"

	srv.CreateStream("test")

	return srv
}

func newServer401() *Server {
	srv = New()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	server = httptest.NewServer(mux)
	urlPath = server.URL + "/events"

	srv.CreateStream("test")

	return srv
}

func publishMsgs(s *Server, empty bool, count int) {
	for a := 0; a < count; a++ {
		if empty {
			s.Publish("test", &Event{Data: []byte("\n")})
		} else {
			s.Publish("test", &Event{Data: []byte("ping")})
		}
		time.Sleep(time.Millisecond * 50)
	}
}

func publishMultilineMessages(s *Server, count int) {
	for a := 0; a < count; a++ {
		s.Publish("test", &Event{ID: []byte("123456"), Data: []byte(mldata)})
	}
}

func cleanup() {
	server.CloseClientConnections()
	server.Close()
	srv.Close()
}

func TestClientSubscribe(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *Event) {
			if msg.Data != nil {
				events <- msg
				return
			}
		})
	}()

	for i := 0; i < 5; i++ {
		msg, err := wait(events, time.Second*1)
		require.Nil(t, err)
		assert.Equal(t, []byte(`ping`), msg)
	}

	assert.Nil(t, cErr)
}

func TestClientSubscribeMultiline(t *testing.T) {
	setupMultiline()
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	var cErr error

	go func() {
		cErr = c.Subscribe("test", func(msg *Event) {
			if msg.Data != nil {
				events <- msg
				return
			}
		})
	}()

	for i := 0; i < 5; i++ {
		msg, err := wait(events, time.Second*1)
		require.Nil(t, err)
		assert.Equal(t, []byte(mldata), msg)
	}

	assert.Nil(t, cErr)
}

func TestClientChanSubscribeEmptyMessage(t *testing.T) {
	setup(true)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	for i := 0; i < 5; i++ {
		_, err := waitEvent(events, time.Second)
		require.Nil(t, err)
	}
}

func TestClientChanSubscribe(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	for i := 0; i < 5; i++ {
		msg, merr := wait(events, time.Second*1)
		if msg == nil {
			i--
			continue
		}
		assert.Nil(t, merr)
		assert.Equal(t, []byte(`ping`), msg)
	}
	c.Unsubscribe(events)
}

func TestClientOnDisconnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	called := make(chan struct{})
	c.OnDisconnect(func(client *Client) {
		called <- struct{}{}
	})

	go c.Subscribe("test", func(msg *Event) {})

	time.Sleep(time.Second)
	server.CloseClientConnections()

	assert.Equal(t, struct{}{}, <-called)
}

func TestClientOnConnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	called := make(chan struct{})
	c.OnConnect(func(client *Client) {
		called <- struct{}{}
	})

	go c.Subscribe("test", func(msg *Event) {})

	time.Sleep(time.Second)
	assert.Equal(t, struct{}{}, <-called)

	server.CloseClientConnections()
}

func TestClientChanReconnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	for i := 0; i < 10; i++ {
		if i == 5 {
			// kill connection
			server.CloseClientConnections()
		}
		msg, merr := wait(events, time.Second*1)
		if msg == nil {
			i--
			continue
		}
		assert.Nil(t, merr)
		assert.Equal(t, []byte(`ping`), msg)
	}
	c.Unsubscribe(events)
}

func TestClientUnsubscribe(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	time.Sleep(time.Millisecond * 500)

	go c.Unsubscribe(events)
	go c.Unsubscribe(events)
}

func TestClientUnsubscribeNonBlock(t *testing.T) {
	count := 2
	setupCount(false, count)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	// Read count messages from the channel
	for i := 0; i < count; i++ {
		msg, merr := wait(events, time.Second*1)
		assert.Nil(t, merr)
		assert.Equal(t, []byte(`ping`), msg)
	}
	// No more data is available to be read in the channel
	// Make sure Unsubscribe returns quickly
	doneCh := make(chan *Event)
	go func() {
		var e Event
		c.Unsubscribe(events)
		doneCh <- &e
	}()
	_, merr := wait(doneCh, time.Millisecond*100)
	assert.Nil(t, merr)
}

func TestClientUnsubscribe401(t *testing.T) {
	srv = newServer401()
	defer cleanup()

	c := NewClient(urlPath)

	// limit retries to 3
	c.ReconnectStrategy = backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(),
		3,
	)

	err := c.SubscribeRaw(func(ev *Event) {
		// this shouldn't run
		assert.False(t, true)
	})

	require.NotNil(t, err)
}

func TestClient204NoReconnect(t *testing.T) {
	// HTTP 204 must stop reconnection immediately — spec §9.2.3.
	var requests int
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	err := c.SubscribeRaw(func(ev *Event) {})

	require.Error(t, err)
	assert.Equal(t, 1, requests, "client must not reconnect after 204")
}

func TestClientLargeData(t *testing.T) {
	srv = newServer()
	defer cleanup()

	c := NewClient(urlPath, ClientMaxBufferSize(1<<19))

	// limit retries to 3
	c.ReconnectStrategy = backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(),
		3,
	)

	// allocate 128KB of data to send
	data := make([]byte, 1<<17)
	rand.Read(data)
	data = []byte(hex.EncodeToString(data))

	ec := make(chan *Event, 1)

	srv.Publish("test", &Event{Data: data})

	go func() {
		c.Subscribe("test", func(ev *Event) {
			ec <- ev
		})
	}()

	d, err := wait(ec, time.Second)
	require.Nil(t, err)
	require.Equal(t, data, d)
}

func TestClientComment(t *testing.T) {
	srv = newServer()
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	srv.Publish("test", &Event{Comment: []byte("comment")})
	srv.Publish("test", &Event{Data: []byte("test")})

	ev, err := waitEvent(events, time.Second*1)
	assert.Nil(t, err)
	assert.Equal(t, []byte("test"), ev.Data)

	c.Unsubscribe(events)
}

func TestTrimHeader(t *testing.T) {
	tests := []struct {
		input []byte
		want  []byte
	}{
		{
			input: []byte("data: real data"),
			want:  []byte("real data"),
		},
		{
			input: []byte("data:real data"),
			want:  []byte("real data"),
		},
		{
			input: []byte("data:"),
			want:  []byte(""),
		},
	}

	for _, tc := range tests {
		got := trimHeader(len(headerData), tc.input)
		require.Equal(t, tc.want, got)
	}
}

func TestSubscribeWithContextDone(t *testing.T) {
	setup(false)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())

	var n1 = runtime.NumGoroutine()

	c := NewClient(urlPath)

	for i := 0; i < 10; i++ {
		go c.SubscribeWithContext(ctx, "test", func(msg *Event) {})
	}

	time.Sleep(1 * time.Second)
	cancel()

	time.Sleep(1 * time.Second)
	var n2 = runtime.NumGoroutine()

	// Goroutines spawned by Subscribe must all exit after context cancellation (no leak).
	// n2 <= n1 proves this: no accumulation. n2 < n1 is also acceptable — background
	// goroutines from previous tests may finish during the measurement window.
	assert.LessOrEqual(t, n2, n1)
}

func TestResponseBodyClosedOnValidatorError(t *testing.T) {
	closeCalled := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	client.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)
	client.ResponseValidator = func(c *Client, resp *http.Response) error {
		original := resp.Body
		resp.Body = &bodyCloseTracker{ReadCloser: original, closed: closeCalled}
		return fmt.Errorf("validator rejected response")
	}

	err := client.Subscribe("", func(msg *Event) {})
	assert.Error(t, err)

	select {
	case <-closeCalled:
		// good
	case <-time.After(time.Second):
		t.Fatal("response body was not closed after validator error")
	}
}

func TestClientReconnectsAfterEOF(t *testing.T) {
	// Server sends one event per connection then closes cleanly (EOF).
	// Client must reconnect and receive a second event from the next connection.
	var mu sync.Mutex
	conns := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()

		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: conn%d\n\n", n)
		flusher.Flush()
		// Returning from the handler closes the response body → client receives EOF.
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop the subscription goroutine when the test ends

	c := NewClient(srv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 5)

	events := make(chan *Event, 10)
	go c.SubscribeRawWithContext(ctx, func(msg *Event) {
		events <- msg
	})

	ev1, err := waitEvent(events, 2*time.Second)
	require.Nil(t, err, "timed out waiting for first event")
	assert.Equal(t, []byte("conn1"), ev1.Data)

	// Without the fix, this blocks forever — the client does not reconnect after EOF.
	ev2, err := waitEvent(events, 2*time.Second)
	require.Nil(t, err, "timed out waiting for second event — client did not reconnect after EOF")
	assert.Equal(t, []byte("conn2"), ev2.Data)
}

func TestClientUnsubscribeWhileBackingOff(t *testing.T) {
	// When the client is in a reconnect backoff sleep (between retries),
	// Unsubscribe must return immediately — not block until the sleep expires.
	//
	// Use an isolated server that accepts the SSE connection but sends no
	// events, so the connection stays open and idle until we close it. This
	// reliably puts the goroutine into the backoff select after disconnect.
	connected := make(chan struct{}, 1)
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		connected <- struct{}{}
		<-r.Context().Done() // hold open until client disconnects
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	// Long constant backoff so the goroutine is guaranteed to be sleeping
	// when we call Unsubscribe.
	c.ReconnectStrategy = backoff.NewConstantBackOff(10 * time.Second)

	events := make(chan *Event)
	err := c.SubscribeChan("", events)
	require.Nil(t, err)

	// Wait for the server to confirm the connection is live.
	<-connected

	// Force the client into a reconnect cycle.
	tsrv.CloseClientConnections()

	// Give the goroutine time to enter the backoff sleep.
	time.Sleep(150 * time.Millisecond)

	// Unsubscribe must not block even though the goroutine is sleeping.
	done := make(chan struct{})
	go func() {
		c.Unsubscribe(events)
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Fatal("Unsubscribe blocked while client was in reconnect backoff")
	}
}

func TestClientDoubleUnsubscribeNoDeadlock(t *testing.T) {
	// Two concurrent Unsubscribe calls must both return without deadlocking.
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.Unsubscribe(events) }()
	go func() { defer wg.Done(); c.Unsubscribe(events) }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		// good — both calls returned
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Unsubscribe calls deadlocked")
	}
}

type bodyCloseTracker struct {
	io.ReadCloser
	closed chan struct{}
}

func (b *bodyCloseTracker) Close() error {
	select {
	case b.closed <- struct{}{}:
	default:
	}
	return b.ReadCloser.Close()
}
