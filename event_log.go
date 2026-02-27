/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"strconv"
	"time"
)

// EventLog holds all of previous events.
type EventLog struct {
	// MaxEntries limits the number of events retained. When a new event
	// would exceed this cap the oldest entry is evicted. Zero means unlimited.
	MaxEntries int
	entries    []*Event
}

// Add event to eventlog.
func (e *EventLog) Add(ev *Event) {
	if !ev.hasContent() {
		return
	}

	ev.ID = []byte(e.currentindex())
	ev.timestamp = time.Now()
	e.entries = append(e.entries, ev)

	if e.MaxEntries > 0 && len(e.entries) > e.MaxEntries {
		e.entries = e.entries[len(e.entries)-e.MaxEntries:]
	}
}

// Clear events from eventlog.
func (e *EventLog) Clear() {
	e.entries = nil
}

// Replay events to a subscriber.
// The send is non-blocking: if the subscriber's channel is full the event is
// skipped rather than stalling the stream's run() goroutine (sse-xua).
func (e *EventLog) Replay(s *Subscriber) {
	if e == nil {
		return
	}
	for i := 0; i < len(e.entries); i++ {
		if e.entries[i] == nil {
			continue
		}
		id, err := strconv.Atoi(string(e.entries[i].ID))
		if err != nil {
			continue
		}
		if id >= s.eventid {
			select {
			case s.connection <- e.entries[i]:
			default:
				// Subscriber channel full — skip rather than block.
			}
		}
	}
}

// Len returns the number of events in the log.
func (e *EventLog) Len() int {
	return len(e.entries)
}

func (e *EventLog) currentindex() string {
	return strconv.Itoa(len(e.entries))
}
