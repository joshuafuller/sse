/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Shared test helpers for package sse_test.
package sse_test

import (
	"errors"
	"time"

	sse "github.com/joshuafuller/sse/v3"
)

// wait blocks until an event arrives on ch or duration elapses.
// It returns the event's Data on success, or an error on timeout.
func wait(ch chan *sse.Event, duration time.Duration) ([]byte, error) {
	select {
	case event := <-ch:
		return event.Data, nil
	case <-time.After(duration):
		return nil, errors.New("timeout")
	}
}

// waitEvent blocks until an event arrives on ch or duration elapses.
func waitEvent(ch chan *sse.Event, duration time.Duration) (*sse.Event, error) {
	select {
	case event := <-ch:
		return event, nil
	case <-time.After(duration):
		return nil, errors.New("timeout")
	}
}
