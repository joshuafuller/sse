/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// White-box tests for http.go that require access to unexported symbols
// (errWriter). Named *_internal_test.go per the testpackage linter
// convention so they are allowed to stay in package sse.
package sse

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// errAfterNWriter is a ResponseWriter that returns an error after n bytes.
type errAfterNWriter struct {
	http.ResponseWriter
	remaining int
}

func (e *errAfterNWriter) Write(p []byte) (int, error) {
	if len(p) > e.remaining {
		e.remaining = 0
		return 0, errors.New("write error: connection closed")
	}
	e.remaining -= len(p)
	return e.ResponseWriter.Write(p)
}

func TestErrWriterStopsOnError(t *testing.T) {
	t.Parallel()

	w := &errAfterNWriter{
		ResponseWriter: httptest.NewRecorder(),
		remaining:      10, // allow 10 bytes then fail
	}

	ew := &errWriter{w: w}

	// First write should succeed (fits in 10 bytes)
	ew.printf("hi")
	assert.Nil(t, ew.err, "first write should succeed")

	// This write should fail (exceeds remaining bytes)
	ew.printf("this is a long string that exceeds the limit")
	assert.NotNil(t, ew.err, "write past limit should fail")

	prevErr := ew.err

	// Subsequent writes should be no-ops
	ew.printf("more data")
	assert.Equal(t, prevErr, ew.err, "error should not change after first failure")

	ew.print("yet more")
	assert.Equal(t, prevErr, ew.err, "print should also be a no-op after error")
}
