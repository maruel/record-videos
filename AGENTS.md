# AGENTS.md — record-videos

For architecture, data flows, pipe ownership, and resilience design see the
package doc comment at the top of `main.go` and the doc comments on
`runFFMPEGOnce`, `run`, and `processMetadata`.

---

## Key files

| File | Role |
|---|---|
| `main.go` | `run()`, `runFFMPEGOnce()`, CLI flags, signal handling |
| `ffmpeg.go` | ffmpeg command construction, filter graph DSL |
| `motion.go` | `processMetadata`, `filterMotion`, `processMotion`, m3u8 generation |
| `server.go` | HTTP server, MJPEG/JPEG endpoints, HLS file serving |
| `teemime.go` | `teeMimePart` — fan-out of mime-multipart JPEG stream |

---

## Building and testing

```bash
go build ./...
go test ./...
go vet ./...
golangci-lint run
```

Tests do **not** require ffmpeg or a camera. `motion_test.go` covers
`processMetadata` parsing and all `filterMotion` exit paths.

---

## Adding new failure modes

1. Identify whether the failure causes ffmpeg to **exit** or to **stall**.
   - Exit → the restart loop in `run()` already handles it.
   - Stall → lower `motionOptions.keepAlive` or add a secondary watchdog.
2. Write a subtest in `TestFilterMotion` (or `TestProcessMetadata`) that
   exercises the new path.
3. New `motionOptions` fields go before `_`; initialise them in `mainImpl`.
