# Changelog

All notable changes to this fork are documented here. Each entry cross-references
the original upstream [r3labs/sse](https://github.com/r3labs/sse) issue or pull
request that motivated the change, where one exists.

This fork begins at **v3.0.0**. All v2.x tags are inherited from the upstream
repository and are documented in its
[commit history](https://github.com/r3labs/sse/commits/master).

---

## [Unreleased]

---

## [v3.0.6] — 2026-02-27

### Added

- **Testing — 100% statement coverage** (up from 96.2%): Added 19 targeted tests
  covering every previously uncovered branch across `client.go`, `http.go`, and
  `stream.go`. New tests cover: `ErrEmptyEventMessage` on empty input,
  bare field name (no colon) per spec §9.2.6, `trimHeader` short-input path,
  custom `Method`/`Headers` forwarding, `retryDelay` initial-interval propagation,
  `ResponseValidator` permanent-error path, disconnect callback on goroutine exit,
  transport error before first connect, HTTP 204 permanent stop,
  invalid `Content-Type` permanent stop, missing `?stream=` parameter,
  `Server.Headers` written to response, data starting with `:` emitted verbatim,
  and `getSubIndex` not-found path. All tests pass with `-race`.

### Fixed

- **`.gitignore` — coverage artifacts not excluded**: `coverage.html` and
  `coverage.txt` were joined as a single literal entry (`coverage.html\ncoverage.txt`)
  due to a missing newline. Fixed to three separate entries; added `coverage.out`.

---

## [v3.0.5] — 2026-02-27

### Fixed

- **Client — `Connected()` stuck `true` after context cancellation**: `connected.Store(false)`
  and `disconnectcb` were only invoked in one narrow path inside `readLoop` (non-EOF,
  non-context errors with a disconnect callback registered). After a normal context
  cancellation or after all backoff retries were exhausted, `Connected()` remained `true`
  indefinitely and `OnDisconnect` was never called. Fixed by adding a final-cleanup defer
  at the goroutine/function boundary in both `SubscribeWithContext` and
  `SubscribeChanWithContext` that atomically swaps `connected` to `false` and fires
  `disconnectcb` exactly once (using `atomic.Bool.Swap` to avoid double-calling when
  `readLoop`'s legacy path already set it).

- **Server — `Cache-Control` response header corrected to `no-cache`**: The server was
  sending `Cache-Control: no-store` in responses (incorrect). The WHATWG SSE spec §9.2.1
  recommends `no-cache` so intermediaries know not to buffer the stream. Client request
  header was already `no-cache` (correct). Changed `http.go` server response to match.

### Added

- **Testing — 26-test end-to-end smoke suite** (`smoke_test.go`): Comprehensive
  integration tests in `package sse_test` that validate every major API surface against
  the README patterns. Covers: server publish, auto-stream, event TTL, event log replay,
  max subscribers, TryPublish, OnConnect/OnDisconnect callbacks, multi-stream isolation,
  base64 encoding, split-data, per-stream callbacks, reconnect-on-EOF, event log max
  entries, and more. All 26 tests pass with `-race`.

### Documentation

- Migration guide extracted to `docs/migrating-from-v2.md`; README now shows counts
  and a link rather than inlining all before/after examples.

---

## [v3.0.4] — 2026-02-27

### Fixed

- **Server — `EventTTL` dropped all events when `AutoReplay=false`**: `ev.timestamp`
  was only set inside `EventLog.Add()`, which only runs when `AutoReplay=true`.
  With `AutoReplay=false` the timestamp stayed at its zero value, so
  `time.Now().After(zero + TTL)` was always `true` — every event was silently
  dropped regardless of how recently it was published. Fixed by stamping
  `event.timestamp = time.Now()` in `server.process()` unconditionally.
  `EventLog.Add()` continues to overwrite it (at approximately the same time)
  when `AutoReplay=true`, which is harmless. Upstream:
  [r3labs/sse PR #176](https://github.com/r3labs/sse/pull/176).

- **Server — `EventLog.Replay()` blocking send stalled the stream's `run()` goroutine**:
  The direct channel send `s.connection <- ev` inside `Replay()` would block
  indefinitely if a reconnecting subscriber's channel was already full (e.g. a
  slow consumer). Because `Replay()` is called from inside the stream's dispatch
  goroutine, a stalled replay prevented all other subscribers from receiving
  events. Fixed by wrapping the send in a non-blocking `select { … default: }`
  so full subscriber channels are skipped during replay, consistent with the
  non-blocking fan-out used during normal dispatch. Detected by the new
  `blocking-channel-send-in-iteration-loop` semgrep rule.

- **Server — subscriber connection channel hardcoded at `64`**: The subscriber
  `connection` channel was created with a fixed capacity of 64 regardless of
  `Server.BufferSize`. High-volume streams configured with a large buffer could
  fill subscriber channels before the dispatcher had a chance to drain them.
  Fixed by storing `buffSize` on the `Stream` struct (`subscriberBuf`) and
  using it when creating subscriber channels, so both buffers scale together.
  Detected by the new `no-hardcoded-channel-buffer` semgrep rule.

- **Client — `ErrEmptyEventMessage` promoted to package-level sentinel**:
  `processEvent` previously returned `errors.New("event message was empty")`
  inline, creating an anonymous error value that callers could not match with
  `errors.Is`. Promoted to `var ErrEmptyEventMessage = errors.New(…)` so
  callers can detect this condition precisely. Detected by the new
  `inline-errors-new-return` semgrep rule.

### Added

- **CI — library-idiom semgrep rules (L1–L6)**: Eight custom rules now enforce
  Go library best practices in addition to the existing mutex-safety rules:
  - `no-hardcoded-channel-buffer` (L1) — flag `make(chan T, N)` where `N > 1`
    is an integer literal; use named constants.
  - `blocking-channel-send-in-iteration-loop` (L2) — flag direct channel sends
    inside index/range loops without a `select` guard.
  - `no-fmt-print-to-stdout` (L3) — libraries must never call `fmt.Print`,
    `fmt.Printf`, or `fmt.Println`; these write to stdout and cannot be silenced
    by callers.
  - `no-time-sleep-in-library` (L4) — libraries must not call `time.Sleep`;
    callers manage delays via context cancellation and backoff strategies.
  - `no-http-default-client` (L5) — libraries must not use `http.DefaultClient`,
    `http.DefaultTransport`, or the package-level helpers (`http.Get`, etc.);
    callers must supply their own `*http.Client`.
  - `inline-errors-new-return` (L6) — returning `errors.New("…")` inline
    prevents callers from using `errors.Is`; promote to a package-level sentinel.

- **Godoc**: replaced stub comments (`// Stream …`, `// Subscriber …`,
  `// Server Is our main struct.`) with accurate descriptions.

---

## [v3.0.3] — 2026-02-27

### Fixed

- **Client — data race in `SubscribeChanWithContext`**: the goroutine read
  `c.subscribed[ch]` (a map lookup) without holding `c.mu`, racing against
  concurrent map writes from other `SubscribeChanWithContext` calls. Fixed by
  capturing `quitCh := c.subscribed[ch]` once before the goroutine is launched,
  so the goroutine never touches the map.

### Tests

- `TestServerCloseCancelsActiveSubscribers` — verifies `Server.Close()`
  terminates active subscriber connections (covers
  [r3labs/sse#93](https://github.com/r3labs/sse/issues/93)).
- `TestClientCommentLinesIgnored` — verifies SSE comment lines (`: …`) are
  never delivered to the handler (WHATWG SSE §9.2.6; covers
  [r3labs/sse#135](https://github.com/r3labs/sse/issues/135)).
- `TestSubscribeChanConcurrentMapRace` — race-detector regression for the
  concurrent map read fixed above.

---

## [v3.0.2] — 2026-02-26

### Fixed

- **Server — `Cache-Control: no-store`**: changed from `no-cache` to `no-store`
  per WHATWG SSE §9.2.1. The previous value was technically incorrect for an
  event stream.

- **Client — `Connected()` method / data race**: the public `Connected bool`
  field was read and written from multiple goroutines without synchronisation.
  Replaced with an unexported `atomic.Bool` and a `Connected() bool` accessor
  method. **Breaking change** relative to r3labs/sse v2 — see the migration
  guide in the README.

### Added

- **CI — mutex-safety semgrep rules**: four custom rules enforce correct mutex
  patterns across the library (`lock-without-defer`, `rwmutex-rlock-without-defer`,
  `channel-send-while-locked`, `goroutine-spawned-while-locked`).

---

## [v3.0.1] — 2026-02-26

### Fixed

- **Client — base64 decode error wrapping**: `fmt.Errorf` format verb changed
  from `%s` to `%w` so decode errors chain correctly with `errors.Is` /
  `errors.As`.

- **Client — dead nil guard in `trimHeader`**: removed an unreachable
  `data == nil` check (a `[]byte` is never nil when passed from `processEvent`).

### Added

- **CI — Codecov coverage upload**: coverage report uploaded on every push;
  badge added to README.

- **CI — custom semgrep rules**: four library-hygiene rules enforce Go best
  practices (`no-panic-in-library`, `no-log-in-library`, `no-os-exit-in-library`,
  `errorf-wrap-with-w`). Run on every push and PR.

### Tests

- `TestServerProcessEncodeBase64False` / `TestServerProcessEncodeBase64True` —
  `server.process()` base64 path.
- `TestTryPublishSuccess` / `TestTryPublishDrop` / `TestTryPublishNonExistentStream`
  — `TryPublish` happy path and drop-on-full behaviour.
- `TestStreamExistsTrue` / `TestStreamExistsFalse` — `StreamExists`.
- `TestRemoveAllSubscribersWithRemovedChannel` — `removeAllSubscribers` with
  the optional `removed` signalling channel.
- `TestProcessEventBase64Valid` / `TestProcessEventBase64Invalid` — base64
  decode path in `processEvent`.
- `TestTrimHeaderShortData` — short-input guard in `trimHeader`.
- `TestConnectedFieldRace` — race-detector regression for the `atomic.Bool`
  conversion.
- `TestClientSubscribeChanRaw` / `TestClientSubscribeChanRawWithContext` —
  `SubscribeChanRaw` and `SubscribeChanRawWithContext` wrappers.

---

## [v3.0.0] — 2026-02-24

This is the first release of the community fork. It begins from the last
r3labs/sse v2 tag and applies systematic fixes against the WHATWG HTML Living
Standard §9.2 (Server-Sent Events) and the known upstream issue backlog.
Every fix is covered by a regression test verified with `-race`.

### Server

- **Non-blocking fan-out dispatch**: publishing an event to a stream no longer
  stalls when a slow subscriber's buffer is full. The event is dropped for that
  subscriber only; other subscribers and the publisher are unaffected.

- **`id:` field omitted when event has no ID**: the server previously always
  emitted `id: \n`, which corrupted the client's `lastEventID` with an empty
  string on every event. The `id:` line is now omitted entirely unless the event
  carries a non-empty `ID` or has `IDPresent` set.
  ([r3labs/sse#33](https://github.com/r3labs/sse/issues/33))

- **Empty `id:` emits bare `id:\n` to reset client `lastEventID`**: when an
  event has `IDPresent=true` with an empty `ID` value the server now emits a
  bare `id:\n` line per WHATWG SSE §9.2.5, which resets the client's
  last-event-ID buffer to the empty string.
  ([r3labs/sse#33](https://github.com/r3labs/sse/issues/33))

- **Missing stream returns HTTP 404**: with `AutoStream=false`, requesting a
  stream that doesn't exist previously returned a 500 Internal Server Error.
  It now returns 404 Not Found.
  ([r3labs/sse#33](https://github.com/r3labs/sse/issues/33))

- **`Last-Event-ID` accepted as opaque string**: the spec treats the
  `Last-Event-ID` header as an opaque string. The server previously rejected
  non-numeric values with a 400 error; it now accepts any value and uses
  `strconv.Atoi` only as a best-effort replay index.
  ([r3labs/sse#33](https://github.com/r3labs/sse/issues/33))

- **Empty-data events use `continue` not `break`**: a logic error in the
  subscriber dispatch loop used `break` on empty-data events, silently
  terminating the subscriber's event loop on the first empty event. Changed to
  `continue`.
  ([r3labs/sse#163](https://github.com/r3labs/sse/issues/163),
  [r3labs/sse#94](https://github.com/r3labs/sse/issues/94))

- **All `fmt.Fprintf` write errors checked in `ServeHTTP`**: previously write
  errors to the `http.ResponseWriter` were silently ignored, keeping dead
  connections alive indefinitely. The event loop now exits on the first write
  error.

- **`EventLog` bounded by `MaxEntries`**: the event log grew without bound when
  `AutoReplay=true`. A configurable `MaxEntries` field on `EventLog` caps the
  log, evicting the oldest entry when the cap is reached.
  ([r3labs/sse#169](https://github.com/r3labs/sse/issues/169),
  [r3labs/sse#100](https://github.com/r3labs/sse/issues/100),
  [r3labs/sse PR#162](https://github.com/r3labs/sse/pull/162))

- **Per-stream `SetOnSubscribe` / `SetOnUnsubscribe` callbacks**: each `*Stream`
  now exposes `SetOnSubscribe(fn)` and `SetOnUnsubscribe(fn)` for targeted
  per-stream subscribe/unsubscribe notifications. Both fire alongside the
  server-level callbacks.
  ([r3labs/sse#138](https://github.com/r3labs/sse/issues/138),
  [r3labs/sse PR#133](https://github.com/r3labs/sse/pull/133))

- **`OnSubscribeHTTP` callback**: `Server.OnSubscribeHTTP` lets the caller write
  initial SSE events directly to the `http.ResponseWriter` at connect time,
  before the main event loop begins. This also addresses requests for header
  access inside `OnSubscribe`.
  ([r3labs/sse#177](https://github.com/r3labs/sse/issues/177),
  [r3labs/sse#170](https://github.com/r3labs/sse/issues/170),
  [r3labs/sse#132](https://github.com/r3labs/sse/issues/132))

- **`MaxSubscribers` cap on `*Stream`**: when set, `addSubscriber` returns
  `(nil, false)` and `ServeHTTP` responds with HTTP 429 Too Many Requests.
  ([r3labs/sse#172](https://github.com/r3labs/sse/issues/172),
  [r3labs/sse PR#142](https://github.com/r3labs/sse/pull/142))

- **`ServeHTTPWithFlusher` method**: exposes a variant of `ServeHTTP` that
  accepts a separate `http.Flusher`, enabling use with Fiber, fasthttp, and
  other frameworks whose response adapters implement flushing separately from
  `http.ResponseWriter`.
  ([r3labs/sse#139](https://github.com/r3labs/sse/issues/139))

- **Per-subscriber goroutine leak on stream shutdown fixed**: `sub.close()`
  sends on `sub.quit` to deregister with the stream. If the stream's `run()`
  goroutine had already exited, this send would block forever. Fixed by
  selecting on `sub.streamQuit` so the close returns immediately when the
  stream is gone.

- **`server.Close()` closes active subscriber connections**: calling
  `Server.Close()` now closes all subscriber `connection` channels, causing
  `ServeHTTP` to return and the HTTP body to EOF for every connected client.
  ([r3labs/sse#93](https://github.com/r3labs/sse/issues/93))

### Client

- **EOF and server-close trigger reconnect**: `io.EOF` was previously treated as
  a clean exit with no reconnect. It is now treated as a transient error and
  enters the backoff retry loop.
  ([r3labs/sse#76](https://github.com/r3labs/sse/issues/76),
  [r3labs/sse#147](https://github.com/r3labs/sse/issues/147),
  [r3labs/sse#75](https://github.com/r3labs/sse/issues/75),
  [r3labs/sse PR#148](https://github.com/r3labs/sse/pull/148))

- **`io.ErrUnexpectedEOF` triggers silent reconnect**: a keepalive timeout
  produces `io.ErrUnexpectedEOF` from the HTTP transport. This is a transient
  network condition, not a real disconnect. The client now reconnects silently
  without invoking `disconnectcb`.
  ([r3labs/sse#76](https://github.com/r3labs/sse/issues/76))

- **HTTP 204 No Content causes permanent stop**: a 204 response returns a
  `backoff.Permanent` error, preventing the client from reconnecting
  indefinitely to a server that has explicitly signalled end-of-stream.

- **`Content-Type` validated on response**: if the server responds with a
  content-type other than `text/event-stream` the client returns a permanent
  error immediately rather than attempting to parse a non-SSE body.

- **`retry:` field applied as reconnect delay**: the `retry:` field value
  (milliseconds) is parsed and applied as the initial interval of the backoff
  strategy, per WHATWG SSE §9.2.6.

- **`id:` with empty value resets `LastEventID`**: receiving an `id:` line with
  an empty value (distinct from the absence of an `id:` field) now correctly
  resets `LastEventID` to the empty string.

- **`id:` containing U+0000 NULL is ignored**: per WHATWG SSE §9.2.6, an `id:`
  field value containing a NULL character must be ignored entirely. The client
  previously stored it.

- **`SubscribeWithContext` returns `context.Canceled` on cancellation**: when
  the caller's context is cancelled the subscribe methods now return
  `context.Canceled` rather than an opaque backoff error.
  ([r3labs/sse#131](https://github.com/r3labs/sse/issues/131),
  [r3labs/sse PR#146](https://github.com/r3labs/sse/pull/146))

- **`erChan` buffered (size 1)**: the internal error channel between
  `readLoop` and its consumer was unbuffered. When the consumer exited early
  (e.g. on `Unsubscribe`), the `readLoop` goroutine blocked on the send and
  leaked permanently. The channel is now buffered with capacity 1.

- **Scanner buffer overflow returns `bufio.ErrTooLong`**: when an event exceeds
  the configured `maxBufferSize`, the scanner previously swallowed the error and
  returned `io.EOF`, silently discarding all subsequent events. It now surfaces
  `bufio.ErrTooLong`.
  ([r3labs/sse#158](https://github.com/r3labs/sse/issues/158),
  [r3labs/sse#97](https://github.com/r3labs/sse/issues/97))

- **ID-only events not dispatched**: an event block containing only an `id:`
  field (no `data:`, `event:`, or `retry:`) was previously delivered to the
  handler. Per WHATWG SSE §9.2.6, it must update `LastEventID` but must not be
  dispatched.
  ([r3labs/sse#163](https://github.com/r3labs/sse/issues/163))

- **`OnConnect` fires immediately after HTTP 200**: previously `OnConnect` fired
  on the first *event* received, not on the connection itself. It now fires as
  soon as the HTTP 200 response is confirmed.
  ([r3labs/sse#149](https://github.com/r3labs/sse/issues/149),
  [r3labs/sse#150](https://github.com/r3labs/sse/issues/150),
  [r3labs/sse PR#151](https://github.com/r3labs/sse/pull/151),
  [r3labs/sse PR#137](https://github.com/r3labs/sse/pull/137))

- **Non-200 responses return `*StreamError`**: HTTP error responses are wrapped
  in a `*StreamError{StatusCode, Body}` value that callers can inspect with
  `errors.As`.
  ([r3labs/sse#168](https://github.com/r3labs/sse/issues/168),
  [r3labs/sse PR#87](https://github.com/r3labs/sse/pull/87))

- **Backoff resets after each successful connection**: after a successful
  connection, the next reconnect attempt starts from the minimum backoff delay
  rather than continuing the exponential ramp.
  ([r3labs/sse PR#152](https://github.com/r3labs/sse/pull/152))

- **Context passed into `startReadLoop`**: the read loop goroutine holds a
  reference to the caller's context and exits promptly on cancellation,
  eliminating a goroutine leak on `Unsubscribe`.
  ([r3labs/sse PR#178](https://github.com/r3labs/sse/pull/178),
  [r3labs/sse PR#128](https://github.com/r3labs/sse/pull/128),
  [r3labs/sse PR#140](https://github.com/r3labs/sse/pull/140))

- **External errors wrapped with `%w`**: errors from the HTTP layer and other
  external sources are wrapped with `%w` so callers can use `errors.Is` /
  `errors.As`.

- **`Method` and `Body` fields for non-GET SSE**: `Client.Method` (defaults to
  GET) and `Client.Body func() io.Reader` (called before each attempt) enable
  POST-based and other non-GET SSE endpoints, such as LLM streaming APIs.
  ([r3labs/sse#153](https://github.com/r3labs/sse/issues/153),
  [r3labs/sse PR#184](https://github.com/r3labs/sse/pull/184),
  [r3labs/sse PR#181](https://github.com/r3labs/sse/pull/181),
  [r3labs/sse PR#171](https://github.com/r3labs/sse/pull/171),
  [r3labs/sse PR#167](https://github.com/r3labs/sse/pull/167),
  [r3labs/sse PR#164](https://github.com/r3labs/sse/pull/164),
  [r3labs/sse PR#157](https://github.com/r3labs/sse/pull/157))

- **`Last-Event-ID` header value sanitized**: NULL (U+0000), LF (U+000A), and
  CR (U+000D) characters are stripped from the `Last-Event-ID` value before it
  is sent as an HTTP request header, per the Fetch specification.

- **Response body closed on validator error**: when a custom
  `ResponseValidator` returns an error the response body is now properly closed,
  preventing a connection leak.
  ([r3labs/sse#95](https://github.com/r3labs/sse/issues/95))

- **`Unsubscribe` deadlock fixed**: calling `Unsubscribe` while the internal
  goroutine was sleeping in the backoff loop caused a permanent deadlock because
  `cleanup()` tried to close the quit channel while the goroutine held it.
  ([r3labs/sse#89](https://github.com/r3labs/sse/issues/89),
  [r3labs/sse#32](https://github.com/r3labs/sse/issues/32),
  [r3labs/sse#50](https://github.com/r3labs/sse/issues/50),
  [r3labs/sse PR#65](https://github.com/r3labs/sse/pull/65))

- **Concurrent `SubscribeChan` panics fixed**: calling `SubscribeChan` (or
  `SubscribeChanWithContext`) concurrently with the same channel previously
  caused an unsynchronised map write, triggering a panic or silent data
  corruption.
  ([r3labs/sse#48](https://github.com/r3labs/sse/issues/48),
  [r3labs/sse PR#49](https://github.com/r3labs/sse/pull/49))

### Parser

- **Leading UTF-8 BOM stripped**: a UTF-8 byte-order mark (EF BB BF) at the
  start of an event stream is stripped before parsing, per WHATWG SSE §9.1.

- **CRLF treated as single line ending (WHATWG WP-004)**: the event-block
  splitter now recognises all seven double-newline combinations as valid event
  terminators (`\n\n`, `\r\r`, `\n\r`, `\r\n\n`, `\n\r\n`, `\r\n\r`,
  `\r\n\r\n`). Previously only `\n\n` was handled.
  ([r3labs/sse PR#69](https://github.com/r3labs/sse/pull/69))

- **Comment lines (`: …`) silently ignored**: lines beginning with `:` are
  discarded and never delivered to the event handler, per WHATWG SSE §9.2.6.
  ([r3labs/sse#135](https://github.com/r3labs/sse/issues/135),
  [r3labs/sse PR#136](https://github.com/r3labs/sse/pull/136))

### Testing

- `TestSubscribeWithContextDone` rewritten with event-driven synchronisation:
  the original used goroutine-count polling, which was non-deterministic and
  frequently flaky on loaded CI runners.
  ([r3labs/sse#186](https://github.com/r3labs/sse/issues/186),
  [r3labs/sse PR#187](https://github.com/r3labs/sse/pull/187))

---

## Upstream Issues Not Addressed

The following open upstream issues were evaluated and determined to be either
out of scope, documentation-only, or already addressed by existing behaviour:

| Issue | Reason |
|-------|--------|
| [#174](https://github.com/r3labs/sse/issues/174) — document reconnect defaults | Documentation-only; the README now covers reconnect strategy configuration |
| [#161](https://github.com/r3labs/sse/issues/161) — custom `http.Client` | Already supported via `Client.Connection` (inherited from v2) |
| [#100](https://github.com/r3labs/sse/issues/100) — delete old messages after interval | `EventLog.MaxEntries` bounds by count; time-based eviction is a separate feature |
| [#81](https://github.com/r3labs/sse/issues/81) — VPS connectivity | Infrastructure / reverse-proxy configuration; not a library issue |
| [#75](https://github.com/r3labs/sse/issues/75) — `AutoReconnect` option | The client now always reconnects on EOF/ErrUnexpectedEOF; no opt-in flag needed |
| [#70](https://github.com/r3labs/sse/issues/70) — events received in batch | Downstream client buffering (browser, nginx, etc.); not a library issue |

---

[Unreleased]: https://github.com/joshuafuller/sse/compare/v3.0.5...HEAD
[v3.0.5]: https://github.com/joshuafuller/sse/compare/v3.0.4...v3.0.5
[v3.0.4]: https://github.com/joshuafuller/sse/compare/v3.0.3...v3.0.4
[v3.0.3]: https://github.com/joshuafuller/sse/compare/v3.0.2...v3.0.3
[v3.0.2]: https://github.com/joshuafuller/sse/compare/v3.0.1...v3.0.2
[v3.0.1]: https://github.com/joshuafuller/sse/compare/v3.0.0...v3.0.1
[v3.0.0]: https://github.com/joshuafuller/sse/compare/v2.10.0...v3.0.0
