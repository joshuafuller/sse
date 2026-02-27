# SSE Library Requirements Specification

**Version:** 1.0
**Date:** 2026-02-26
**Normative Reference:** WHATWG HTML Living Standard §9.2 — Server-Sent Events
**Source:** `docs/specs/whatwg-server-sent-events.txt`

---

## Conventions

- **MUST** / **MUST NOT** — mandatory requirement
- **SHOULD** / **SHOULD NOT** — strongly recommended
- **MAY** — optional
- Spec section references are written as `[§9.2.x]`
- Each requirement is assigned a stable identifier: `WP-NNN` (Wire Protocol), `CL-NNN` (Client), `SV-NNN` (Server), `NF-NNN` (Non-Functional)

---

## Part 1 — Wire Protocol

These requirements govern the SSE framing format used on the wire. They apply to both the parser (client-side) and the formatter (server-side).

### 1.1 Encoding and MIME Type

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| WP-001 | The event stream MIME type MUST be `text/event-stream`. | §9.2.5 |
| WP-002 | Event streams MUST always be encoded as UTF-8. There is no mechanism to specify an alternative character encoding. | §9.2.5, §9.2.6 |
| WP-003 | A leading UTF-8 Byte Order Mark (U+FEFF BOM), if present, MUST be stripped before any parsing begins. | §9.2.6 |

### 1.2 Line Ending Normalization

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| WP-004 | Lines MUST be terminated by one of: CR+LF (U+000D U+000A), LF alone (U+000A), or CR alone (U+000D). A CR immediately followed by LF is a single line ending, not two. | §9.2.5, §9.2.6 |
| WP-005 | Block buffering or line buffering with incorrect expected line endings MUST NOT be used, as it causes delays in event dispatch. | §9.2.5 |

### 1.3 Line Classification and Parsing

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| WP-006 | An empty line (blank line) MUST trigger event dispatch. | §9.2.6 |
| WP-007 | A line whose first character is U+003A COLON (`:`) is a comment and MUST be silently ignored. | §9.2.6 |
| WP-008 | A line containing a U+003A COLON MUST be split at the **first** colon only. The field name is the substring before the colon; the field value is the substring after the colon. If the value begins with a single U+0020 SPACE, that space MUST be stripped. | §9.2.6 |
| WP-009 | A non-empty line that does not contain a colon MUST be treated as a field whose name is the entire line and whose value is the empty string. | §9.2.6 |
| WP-010 | Field names MUST be compared literally; no case folding is performed. | §9.2.6 |
| WP-011 | Unknown field names MUST be silently ignored. | §9.2.6 |

### 1.4 Field Processing

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| WP-012 | A field named `event` MUST set the event type buffer to the field value. | §9.2.6 |
| WP-013 | A field named `data` MUST append the field value to the data buffer, then append a single U+000A LF character to the data buffer. This applies even when the value is empty (bare `data` with no colon, or `data:` with no value). | §9.2.6 |
| WP-014 | A field named `id` whose value does not contain U+0000 NULL MUST set the last event ID buffer to the field value. If the value contains NULL, the field MUST be silently ignored and the buffer left unchanged. | §9.2.6 |
| WP-015 | A field named `retry` whose value consists entirely of ASCII decimal digits MUST be interpreted as a base-10 integer and used to set the reconnection time. If the value contains any non-digit character, the field MUST be silently ignored. | §9.2.6 |

### 1.5 Event Dispatch

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| WP-016 | On dispatch, the last event ID string MUST be set from the last event ID buffer. The buffer is NOT reset on dispatch; it persists until the server sends a new `id:` field. | §9.2.6 |
| WP-017 | On dispatch, if the data buffer is the empty string, the data buffer and event type buffer MUST be reset to empty and the event MUST NOT be dispatched. | §9.2.6 |
| WP-018 | On dispatch, if the data buffer's last character is U+000A LF, that trailing LF MUST be removed before the event is emitted. | §9.2.6 |
| WP-019 | On dispatch, if the event type buffer is empty, the event type MUST default to `"message"`. | §9.2.6 |
| WP-020 | On dispatch, the data buffer and event type buffer MUST be reset to the empty string. | §9.2.6 |
| WP-021 | If the stream ends (EOF) while an event is being accumulated (i.e., before the terminating blank line), that incomplete event MUST be discarded and MUST NOT be dispatched. | §9.2.6 |

---

## Part 2 — Client

These requirements govern the behavior of the SSE client: connection establishment, response validation, reconnection logic, and the `Last-Event-ID` mechanism.

### 2.1 Request Headers

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| CL-001 | The client MUST send `Accept: text/event-stream` on every request. | §9.2.2 |
| CL-002 | The client MUST send `Cache-Control: no-cache` (or equivalent no-store cache mode) on every request. | §9.2.2 |
| CL-003 | When reconnecting, if the last event ID string is non-empty, the client MUST include a `Last-Event-ID` request header whose value is the last event ID string. | §9.2.4 |
| CL-004 | The `Last-Event-ID` header value MUST NOT contain U+0000 NULL, U+000A LF, or U+000D CR. | §9.2.4 |

### 2.2 Response Validation

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| CL-005 | The client MUST validate that the response `Content-Type` is `text/event-stream`. If it is not, the client MUST fail the connection (permanent, no reconnect). | §9.2.3 |
| CL-006 | The client MUST fail the connection permanently if the server responds with HTTP 204 No Content. | §9.2.3 |
| CL-007 | The client MUST fail the connection (no reconnect) on any HTTP response status other than 200, except for 301/307 redirects which MUST be followed. | §9.2.3 |

### 2.3 Connection Lifecycle

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| CL-008 | On successful connection, the client MUST announce the connection (fire an `open` event / invoke the connected callback). | §9.2.3 |
| CL-009 | On a network error, the client MUST attempt to reestablish the connection, unless the user agent knows that to be futile. | §9.2.3 |
| CL-010 | On EOF / server-side stream close (non-error end of body), the client MUST reestablish the connection. | §9.2.3 |
| CL-011 | Before reestablishing, the client MUST fire an `error` event / invoke the disconnect callback, and MUST wait for the reconnection time before reconnecting. | §9.2.3 |
| CL-012 | The initial reconnection time is implementation-defined; the spec suggests a value in the region of a few seconds. | §9.2.2 |
| CL-013 | The client SHOULD implement exponential backoff on consecutive failures to avoid overloading a struggling server. | §9.2.3 |

### 2.4 Last-Event-ID Semantics

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| CL-014 | The last event ID buffer MUST persist across events. It is only changed when the server sends a new `id:` field. | §9.2.6 |
| CL-015 | An `id:` field with an empty value MUST reset the last event ID string to the empty string. After this, no `Last-Event-ID` header is sent on reconnect. | §9.2.6 |
| CL-016 | An `id:` field whose value contains U+0000 NULL MUST be ignored; the last event ID buffer MUST remain unchanged. | §9.2.6 |
| CL-017 | A `retry:` field value, if valid (all ASCII digits), MUST be applied as the new reconnection time for all subsequent reconnects. | §9.2.6 |

---

## Part 3 — Server

These requirements govern the behavior of the SSE server: response formatting, HTTP handling, and stream management. The WHATWG spec is browser-oriented; requirements here are derived from the wire protocol rules applied to the server role.

### 3.1 HTTP Response Headers

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| SV-001 | The server MUST set `Content-Type: text/event-stream` on the response. | §9.2.5 |
| SV-002 | The server MUST set `Cache-Control: no-cache` on the response. | §9.2.5 |
| SV-003 | The server MUST flush each event to the client immediately (not buffer across events). | §9.2.5 note |
| SV-004 | The server SHOULD set `Connection: keep-alive` when using HTTP/1.1. | §9.2.7 |

### 3.2 Event Formatting

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| SV-005 | Events MUST be terminated by a blank line (two consecutive LF characters). | §9.2.5 |
| SV-006 | The `id:` field MUST only be emitted when the event has a non-empty ID. Emitting `id:` for events that have no ID will corrupt the client's last event ID state. | §9.2.6 |
| SV-007 | To reset a client's last event ID, the server MUST send `id:` with an empty value (no trailing content). | §9.2.6 |
| SV-008 | Multi-line data MUST be sent as multiple `data:` fields, one per line. | §9.2.6 |
| SV-009 | Comment / keepalive lines MUST be formatted as `: <text>` (colon, optional space, text) followed by a line ending. | §9.2.6 |
| SV-010 | The server SHOULD send a keepalive comment line approximately every 15 seconds to prevent proxy timeouts. | §9.2.7 |
| SV-011 | A `retry:` field emitted by the server MUST contain only ASCII decimal digits. | §9.2.6 |

### 3.3 HTTP Request Handling

| ID | Requirement | Spec Ref |
|----|-------------|----------|
| SV-012 | The server MUST treat the `Last-Event-ID` request header value as an opaque string. It MUST NOT require it to be numeric. | §9.2.4 |
| SV-013 | The server MAY respond with HTTP 204 No Content to instruct the client to stop reconnecting permanently. | §9.2.3 |
| SV-014 | When a requested stream does not exist and `AutoStream` is disabled, the server SHOULD respond with a 4xx status (e.g., 404 Not Found), not 5xx. | — |
| SV-015 | The server MUST support the `http.Flusher` interface; if the `ResponseWriter` does not implement it, the server MUST return an appropriate error. | §9.2.5 |

---

## Part 4 — Non-Functional Requirements

### 4.1 Concurrency and Safety

| ID | Requirement |
|----|-------------|
| NF-001 | All server stream operations (publish, subscribe, unsubscribe, close) MUST be goroutine-safe. |
| NF-002 | Subscriber registration and removal MUST NOT cause data races under concurrent access. |
| NF-003 | Context cancellation MUST promptly stop all goroutines associated with that context, without leaking. |
| NF-004 | Calling `Unsubscribe` MUST NOT deadlock, regardless of the state of the subscriber or its goroutines. |
| NF-005 | Server `Close()` MUST drain and shut down all streams and subscribers without goroutine leaks. |

### 4.2 Memory Bounds

| ID | Requirement |
|----|-------------|
| NF-006 | The `EventLog` MUST NOT grow unboundedly. It MUST support a configurable maximum size and/or TTL-based eviction. |
| NF-007 | The event stream scanner buffer MUST have a configurable maximum size to prevent OOM on malformed or adversarial input. |
| NF-008 | Connection close (client disconnect or server shutdown) MUST release all subscriber resources, including channel allocations. |

### 4.3 Error Handling

| ID | Requirement |
|----|-------------|
| NF-009 | All errors returned by `io.Writer` calls in `ServeHTTP` (e.g., `fmt.Fprintf` to `ResponseWriter`) MUST be checked and acted upon. |
| NF-010 | Errors originating from external packages MUST be wrapped with contextual information before being returned to the caller. |
| NF-011 | A scanner buffer overflow (token too long) MUST NOT cause silent re-subscription or data loss; the error MUST be surfaced to the caller. |

### 4.4 Testing

| ID | Requirement |
|----|-------------|
| NF-012 | All public API surface MUST have test coverage. |
| NF-013 | Tests MUST be deterministic; no race conditions, no timing-dependent failures. |
| NF-014 | Tests SHOULD reside in an external test package (`sse_test`) to enforce public API boundaries. |

---

## Compliance Matrix

Current compliance status as of 2026-02-26. Status definitions:

- `COMPLIANT` — requirement is met by the current implementation
- `NON-COMPLIANT` — known defect; implementation does not meet the requirement
- `PARTIAL` — requirement is partially met; edge cases are wrong
- `UNTESTED` — likely correct but no test coverage to verify
- `N/A` — not applicable to this library in its Go context

| ID | Summary | Status | Beads Issue |
|----|---------|--------|-------------|
| WP-001 | MIME type is `text/event-stream` | COMPLIANT | — |
| WP-002 | Stream is UTF-8 | COMPLIANT | — |
| WP-003 | Strip leading BOM | COMPLIANT | sse-cye (fixed) |
| WP-004 | CR/LF/CRLF line endings | PARTIAL | — |
| WP-005 | No block buffering | COMPLIANT | — |
| WP-006 | Blank line triggers dispatch | COMPLIANT | — |
| WP-007 | Comment lines ignored | COMPLIANT | — |
| WP-008 | Split at first colon only; strip one leading space | COMPLIANT | — |
| WP-009 | Line without colon → empty value | COMPLIANT | — |
| WP-010 | Field names compared literally | COMPLIANT | — |
| WP-011 | Unknown fields ignored | COMPLIANT | — |
| WP-012 | `event` field sets type buffer | COMPLIANT | — |
| WP-013 | `data` field appends + LF (bare `data` case) | COMPLIANT | sse-pc8 (fixed) |
| WP-014 | `id` containing NULL ignored; buffer unchanged | COMPLIANT | sse-coz (fixed) |
| WP-015 | `retry` only applied if all-digit | COMPLIANT | sse-3we (fixed) |
| WP-016 | Last event ID buffer persists across events | COMPLIANT | sse-2mm (fixed) |
| WP-017 | Empty data buffer → no dispatch | COMPLIANT | sse-pc8 (fixed) |
| WP-018 | Strip trailing LF from data on dispatch | COMPLIANT | — |
| WP-019 | Default event type is `"message"` | COMPLIANT | — |
| WP-020 | Reset data/event buffers after dispatch | COMPLIANT | — |
| WP-021 | Incomplete event at EOF discarded | COMPLIANT | sse-fyk (fixed: scanner discards partial events; reconnect on io.EOF and io.ErrUnexpectedEOF) |
| CL-001 | Send `Accept: text/event-stream` | COMPLIANT | — |
| CL-002 | Send `Cache-Control: no-cache` | COMPLIANT | — |
| CL-003 | Send `Last-Event-ID` header when non-empty | COMPLIANT | — |
| CL-004 | `Last-Event-ID` value must not contain NULL/LF/CR | UNTESTED | — |
| CL-005 | Validate `Content-Type: text/event-stream` | COMPLIANT | sse-fk2 (fixed) |
| CL-006 | HTTP 204 → fail permanently | COMPLIANT | sse-a67 (fixed) |
| CL-007 | Non-200 (non-redirect) → fail connection | COMPLIANT | sse-6v2 (fixed) |
| CL-008 | Announce connection on open | COMPLIANT | sse-6v2 (fixed) |
| CL-009 | Network error → reestablish | COMPLIANT | — |
| CL-010 | EOF → reestablish | COMPLIANT | sse-fyk, sse-nsj (fixed) |
| CL-011 | Fire error callback before reconnect | COMPLIANT | sse-fyk (fixed: ErrUnexpectedEOF silent, real errors fire disconnectcb) |
| CL-012 | Initial reconnection time implementation-defined | COMPLIANT | — |
| CL-013 | Exponential backoff on failure | COMPLIANT | — |
| CL-014 | Last event ID persists across events | COMPLIANT | sse-2mm (fixed) |
| CL-015 | Empty `id:` resets last event ID | COMPLIANT | sse-2mm (fixed) |
| CL-016 | `id:` with NULL ignored | COMPLIANT | sse-coz (fixed) |
| CL-017 | `retry:` applied as new reconnection time | COMPLIANT | sse-3we (fixed) |
| SV-001 | Set `Content-Type: text/event-stream` | COMPLIANT | — |
| SV-002 | Set `Cache-Control: no-cache` | COMPLIANT | — |
| SV-003 | Flush each event immediately | COMPLIANT | — |
| SV-004 | Set `Connection: keep-alive` | COMPLIANT | — |
| SV-005 | Events terminated by blank line | COMPLIANT | — |
| SV-006 | `id:` only emitted when event has non-empty ID | COMPLIANT | sse-0s5 (fixed) |
| SV-007 | Empty `id:` resets client's last event ID | UNTESTED | — |
| SV-008 | Multi-line data → multiple `data:` fields | COMPLIANT | — |
| SV-009 | Comment/keepalive line format | COMPLIANT | — |
| SV-010 | Send keepalive comment ~every 15s | N/A | — |
| SV-011 | `retry:` contains only ASCII digits | UNTESTED | — |
| SV-012 | `Last-Event-ID` treated as opaque string (not numeric) | COMPLIANT | sse-2et (fixed) |
| SV-013 | HTTP 204 to stop reconnecting | N/A | — |
| SV-014 | Unknown stream → 4xx not 5xx | COMPLIANT | sse-2et (fixed) |
| SV-015 | Requires `http.Flusher` support | COMPLIANT | — |
| NF-001 | Stream operations goroutine-safe | COMPLIANT | sse-jsb (fixed) |
| NF-002 | Subscriber add/remove no data races | PARTIAL | sse-svu |
| NF-003 | Context cancel stops all goroutines | COMPLIANT | sse-1vw, sse-c6q (fixed: context propagated into readLoop) |
| NF-004 | Unsubscribe does not deadlock | COMPLIANT | sse-vuw (fixed) |
| NF-005 | Server Close() no goroutine leaks | PARTIAL | sse-svu |
| NF-006 | EventLog bounded (max size / TTL) | COMPLIANT | sse-eg7 (MaxEntries), sse-bbt (EventTTL field in Server — both done) |
| NF-007 | Scanner buffer configurable max size | COMPLIANT | sse-5xl (ClientMaxBufferSize functional option — already implemented) |
| NF-008 | Connection close releases all resources | PARTIAL | sse-svu |
| NF-009 | Writer errors checked in ServeHTTP | COMPLIANT | sse-6lv (fixed) |
| NF-010 | External errors wrapped with context | COMPLIANT | sse-71z (fixed: errors wrapped with %w) |
| NF-011 | Scanner overflow surfaced (not silent) | COMPLIANT | sse-2e2 (fixed) |
| NF-012 | Public API has test coverage | COMPLIANT | — |
| NF-013 | Tests are deterministic | COMPLIANT | sse-zfc (fixed, event-driven sync) |
| NF-014 | Tests in external package (`sse_test`) | PARTIAL | sse-4pn (event_log_test.go migrated; others blocked on unexported symbols) |
