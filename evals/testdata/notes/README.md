# notes

A tiny in-memory note buffer that flushes batches to a sink.

- `store.go` - the buffer itself: `Add`, `Drain`, `Close`.
- `flush.go` - `Flush`, which drains the buffer and retries the sink.

Callers are expected to recieve an error from `Add` once the store is closed.
