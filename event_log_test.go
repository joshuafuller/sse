/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	sse "github.com/joshuafuller/sse/v3"
)

func TestEventLog(t *testing.T) {
	ev := &sse.EventLog{}
	testEvent := &sse.Event{Data: []byte("test")}

	ev.Add(testEvent)
	ev.Clear()

	assert.Equal(t, 0, ev.Len())

	ev.Add(testEvent)
	ev.Add(testEvent)

	assert.Equal(t, 2, ev.Len())
}

func TestEventLogMaxEntries(t *testing.T) {
	ev := &sse.EventLog{MaxEntries: 5}

	for i := 0; i < 10; i++ {
		ev.Add(&sse.Event{Data: []byte("event")})
	}

	assert.Equal(t, 5, ev.Len())
}
