/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Network-level smoke tests: TLS and authentication headers.
package sse_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmokeHTTPS verifies full SSE subscription over HTTPS using a self-signed
// test certificate provided by httptest.NewTLSServer.
func TestSmokeHTTPS(t *testing.T) {
	sseServer := sse.New()
	defer sseServer.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(sseServer.ServeHTTP))
	defer tlsSrv.Close()

	sseServer.CreateStream("tls-test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *sse.Event, 4)
	client := sse.NewClient(tlsSrv.URL + "?stream=tls-test")
	client.Connection = tlsSrv.Client()
	client.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)
	go func() { _ = client.SubscribeChanRawWithContext(ctx, events) }()

	require.Eventually(t, func() bool {
		return sseServer.GetStream("tls-test").SubscriberCount() > 0
	}, time.Second, 10*time.Millisecond, "client did not connect over TLS")

	sseServer.Publish("tls-test", &sse.Event{Data: []byte("secure-ping")})

	select {
	case ev := <-events:
		assert.Equal(t, []byte("secure-ping"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event over HTTPS")
	}
}

// TestSmokeAuthHeader proves that Client.Headers are forwarded on every request.
// A raw HTTP handler checks for the expected Authorization header and returns 401
// if it is missing or incorrect.
func TestSmokeAuthHeader(t *testing.T) {
	const token = "Bearer smoke-test-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()

		_, _ = fmt.Fprint(w, "data: authenticated\n\n")
		f.Flush()

		// Block until the client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan *sse.Event, 4)
	client := sse.NewClient(srv.URL)
	client.Headers = map[string]string{"Authorization": token}
	client.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	go func() {
		_ = client.SubscribeRawWithContext(ctx, func(ev *sse.Event) {
			received <- ev
		})
	}()

	select {
	case ev := <-received:
		assert.Equal(t, []byte("authenticated"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for authenticated event")
	}
}
