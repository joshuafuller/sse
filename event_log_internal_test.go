/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// White-box tests for event_log.go that require access to unexported fields.
// Named *_internal_test.go per the testpackage linter convention so they are
// allowed to stay in package sse.
package sse

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestReplaySkipsNilEntries verifies sse-45h: if the entries slice contains a
// nil *Event (defensive case — Add() never inserts nil, but guard anyway),
// Replay must not panic.
func TestReplaySkipsNilEntries(t *testing.T) {
	el := &EventLog{}
	// Inject a nil entry directly, bypassing Add() which would reject it.
	el.entries = []*Event{nil, {Data: []byte("hello")}, nil}

	sub := &Subscriber{
		connection: make(chan *Event, 8),
	}

	assert.NotPanics(t, func() {
		el.Replay(sub)
	})
}

// TestReplaySkipsUnparsableID verifies sse-45h: entries whose ID cannot be
// parsed as an integer must be skipped rather than silently treated as id==0.
func TestReplaySkipsUnparsableID(t *testing.T) {
	el := &EventLog{}
	// Inject entries with an unparsable ID directly.
	el.entries = []*Event{
		{ID: []byte("not-a-number"), Data: []byte("skip me")},
		{ID: []byte("1"), Data: []byte("keep me")},
	}

	sub := &Subscriber{
		connection: make(chan *Event, 8),
		eventid:    0,
	}

	assert.NotPanics(t, func() {
		el.Replay(sub)
	})

	// Only the entry with a valid integer ID >= 0 should be delivered.
	assert.Equal(t, 1, len(sub.connection))
	ev := <-sub.connection
	assert.Equal(t, []byte("keep me"), ev.Data)
}
