# Prototype 01: File Upload with SSE Progress Tracking — Complete Journey

## Preface

This is the full record of building Prototype 01 of the HTTP Protocol Deep Dive project — a file upload service with real-time SSE progress streaming. It was built in Go from scratch, one decision at a time, guided by protocol-level thinking rather than framework convenience.

The goal was never "make file upload work." The goal was to understand what is actually happening at the wire, in RAM, in the kernel, and in the HTTP spec — and to be able to explain it clearly afterward.

---

## Project Philosophy

The project philosophy has three rules:

1. **Protocol-layer depth first.** Understand what's on the wire before reaching for abstractions.
2. **Memory flow matters.** At every step, ask: where is this data living? In the kernel? In a user-space buffer? On disk?
3. **Build observability in from day one.** If you can't see it, you don't understand it.

Each prototype ends with a Substack article. The implementation forces the understanding; the article proves it.

---

## What We Built

A three-endpoint HTTP service:

```
POST /upload/initiate         → registers upload, returns {upload_id, upload_url}
PUT  /upload/{uploadid}       → receives file, streams to disk, throttled via token bucket
GET  /upload/progress/{uploadid}  → SSE stream of upload progress, polling every 200ms
```

The design intentionally avoids pre-signed URLs, message queues, and any "production shortcut." The point is to understand the HTTP machinery directly.

---

## Phase 1: Content-Type Validation

### The First Question

The first real question was: how do I validate that the incoming request is `multipart/form-data`?

The first instinct was `http.DetectContentType`. Read the body, pass it in, get back a MIME type. Makes sense — until you think about what that function actually does.

`http.DetectContentType` sniffs the first 512 bytes of the content to guess the type. Two problems:
1. It would consume bytes from the request body — and `net/http` does not let you seek back.
2. It doesn't know about `multipart/form-data`. It returns `text/plain` for multipart content because it looks like text.

The correct approach: read the `Content-Type` request header. The client declares the content type there. That's the contract.

But "just read the header" is too simple. `Content-Type` for multipart has a parameter:

```
Content-Type: multipart/form-data; boundary=----WebKitFormBoundary7MA4YWxkTrZu0gW
```

The boundary is required — it's how the server knows where one part ends and the next begins. So validation isn't just checking the media type. It's:

1. Parse the header with `mime.ParseMediaType`
2. Verify media type is `multipart/form-data`
3. Verify `boundary` parameter is present

```go
mtype, params, err := mime.ParseMediaType(ctype)
if err != nil { ... }
if mtype != "multipart/form-data" { ... }
if _, ok := params["boundary"]; !ok { ... }
```

**Protocol insight:** The boundary can be any random string. The spec doesn't require `------` prefixes — that's just curl's convention. The server doesn't generate or validate the boundary value itself; it just passes it to the multipart reader.

### The `Content-Disposition` Bug

Inside multipart parts, there's a `Content-Disposition` header:

```
Content-Disposition: form-data; name="file"; filename="upload.csv"
```

The code to extract the filename was failing silently. Every request returned a 400 with "invalid content-disposition." The reason: the header name was misspelled as `content-desposition` in the code. `Header.Get` returns `""` for missing keys — no error, no panic. The empty string passed to `mime.ParseMediaType` returned an error, and every request was rejected.

**Lesson:** Go's HTTP headers are case-insensitive but spelling-sensitive. Silent empty string returns are a class of bug — log the values, not just the errors.

---

## Phase 2: Streaming Upload to Disk

### The Core Decision: Stream or Buffer?

`net/http` gives you two ways to handle multipart:

- `r.ParseMultipartForm(maxMemory)` — reads the entire multipart body, buffers up to `maxMemory` in RAM, spills the rest to a temp file. You get a `map[string][]*FileHeader` at the end.
- `r.MultipartReader()` — returns a streaming reader. You call `reader.NextPart()` to get each part one at a time, then read from it like any `io.Reader`.

For a file upload service that might receive 8GB files, `ParseMultipartForm` is a non-starter. The streaming approach:

```go
reader, err := r.MultipartReader()
for {
    part, err := reader.NextPart()
    if err == io.EOF { break }
    // read from part
}
```

### The Memory Flow

This is the key mental model for the whole prototype:

```
Internet
  → ENI (elastic network interface)
  → NIC
  → DMA ring buffer (kernel memory, zero-copy from NIC)
  → TCP stack (reassembly, in kernel)
  → Socket receive buffer (kernel memory, ~128KB–256KB default)
  → io.Copy read buffer (user-space, 32KB)
  → disk (via os.File write)
```

For an 8GB upload, the amount of data in RAM at any given moment is approximately:
- Socket buffer: ~128KB (configurable, kernel-managed)
- io.Copy buffer: 32KB
- Total: ~160KB

The rest is either in transit on the network, already written to disk, or waiting in the OS page cache to be flushed. The key insight: Go's `io.Copy` is a loop — it allocates a 32KB buffer once and reuses it. It doesn't accumulate the entire file in memory.

### `io.Reader` Contract

Understanding `io.Copy` requires understanding the `io.Reader` contract:

```go
type Reader interface {
    Read(p []byte) (n int, err error)
}
```

The **caller** allocates the buffer `p`. The reader fills it. It returns `n` — the number of bytes actually written into `p`. The caller must only look at `p[:n]`, not `p[:]`.

`io.Copy` does this:
```go
buf := make([]byte, 32*1024)  // allocated once
for {
    nr, er := src.Read(buf)
    if nr > 0 {
        nw, ew := dst.Write(buf[0:nr])
        // ...
    }
    if er == io.EOF { break }
}
```

Every call to `src.Read` asks: "give me up to 32KB." The reader gives back however many bytes are available without blocking too long — could be 32KB, could be 1 byte, could be 0 with a non-EOF error. The caller handles all three.

---

## Phase 3: The Custom Mux and Middleware

### Why a Custom Mux?

The standard `http.ServeMux` handler signature is:

```go
func(http.ResponseWriter, *http.Request)
```

There's no error return. Error handling has to happen inside every handler — write a JSON error, return. That pattern is repetitive and error-prone (missing `return` after error response = double `WriteHeader`, superfluous response warnings in logs).

The custom mux uses a typed handler:

```go
type Handler func(context.Context, http.ResponseWriter, *http.Request) error
```

Handlers return errors. The mux (via middleware) decides what to do with them. This separates the "what went wrong" from "how to tell the client."

### The Middleware Chain

```go
type MiddlewareFunc func(h Handler) Handler
```

Middleware wraps a handler. To chain them, you wrap in reverse:

```go
func AddMiddlewares(h Handler, m ...MiddlewareFunc) Handler {
    for i := len(m) - 1; i >= 0; i-- {
        h = m[i](h)
    }
    return h
}
```

**Ordering convention (established deliberately):** Rightmost middleware is closest to the handler. When you write:

```go
mux.HandleFunc("/resource", handler, authn, authz)
```

`authz` runs first, then `authn`, then `handler`. Rightmost = most qualified = runs last on the way in, first on the way out. Think of it like nesting boxes — you open the outermost box first to reach the innermost (handler) box.

This isn't the only convention, but it was established explicitly and documented in code comments so future readers don't second-guess it.

### The Error Middleware

```go
func ErrorMiddleware(log *logger.Logger) mux.MiddlewareFunc {
    return func(h mux.Handler) mux.Handler {
        return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
            err := h(ctx, w, r)
            if err == nil { return nil }
            var apiError apis.Error
            if errors.As(err, &apiError) {
                apis.WriteJson(w, apiError.StatusCode, apis.ApiResponse{Message: apiError.Message})
                return nil
            }
            apis.WriteJson(w, http.StatusInternalServerError, apis.ApiResponse{Message: err.Error()})
            return nil
        }
    }
}
```

Handlers return typed `apis.Error` values — they carry a status code and a user-facing message. The middleware intercepts, checks type, writes the right JSON. Internal errors fall through to a 500.

### The Tracing Middleware and `MuxResponseWriter`

The tracing middleware needed to log the response status code after the handler returned. But `http.ResponseWriter` doesn't expose the status code after the fact. Solution: wrap it.

```go
type MuxResponseWriter struct {
    w          http.ResponseWriter
    StatusCode int
}

func (w *MuxResponseWriter) WriteHeader(statusCode int) {
    w.StatusCode = statusCode
    w.w.WriteHeader(statusCode)
}
```

This exposed a subtle bug: the original implementation called `w.WriteHeader(statusCode)` — recursive call, infinite loop. The fix was `w.w.WriteHeader(statusCode)` — delegate to the underlying writer.

The wrapper also needed to implement `http.Flusher`:

```go
func (w *MuxResponseWriter) Flush() {
    if f, ok := w.w.(http.Flusher); ok {
        f.Flush()
    }
}
```

Without this, any handler that tried to call `w.(http.Flusher).Flush()` would panic — the type assertion would fail because `MuxResponseWriter` didn't implement the interface. SSE requires `Flush()` after every event. This was a real runtime panic before the fix.

### The Implicit 200 Bug (SSE Status Logged as 0)

After the SSE handler was working, the tracing middleware logged `"status": 0` for every `GET /upload/progress/...` request, while all other endpoints logged the correct code.

The cause: the SSE handler never calls `w.WriteHeader(200)` explicitly. It sets headers, then writes directly with `fmt.Fprintf`. Go's `net/http` handles this by implicitly sending a 200 on the first `Write` call — but that implicit call goes through the **underlying** response writer's internal machinery, not through `tracingResponseWriter.WriteHeader`. So `StatusCode` stayed at `0` (the zero value for `int`).

The fix: intercept in `Write`:

```go
func (w *tracingResponseWriter) Write(b []byte) (int, error) {
    if w.StatusCode == 0 {
        w.StatusCode = http.StatusOK
    }
    return w.w.Write(b)
}
```

First write with no prior `WriteHeader` call means the implicit 200. Capture it there, delegate the actual write unchanged. This mirrors what `net/http`'s own internal `response` type does before sending bytes.

After the fix, the logs show the full picture correctly:

```json
{"msg":"Request ended","duration":"8.239s","status":200, "trace":{"request_method":"PUT",...}}
{"msg":"Request ended","duration":"9.832s","status":200, "trace":{"request_method":"GET",...}}
```

The timing also tells the story: the PUT took 8.239s (token bucket), the SSE GET stayed open 1.6s longer — polling every 200ms until it caught the `IsComplete` flag, sent the final event, then closed.

---

## Phase 4: The Progress Store

### Design: In-Memory, Singleton, Mutex-Protected

The progress store is a `map[string]Progress` protected by a `sync.Mutex`. It's a singleton — all upload handlers and SSE handlers share the same instance.

```go
type Progress struct {
    Err        error
    Total      uint64
    SoFar      uint64
    IsComplete bool
}
```

The singleton is initialized with `sync.Once`:

```go
var ups *uploadProgressStore
var once sync.Once

func new() *uploadProgressStore {
    once.Do(func() {
        ups = &uploadProgressStore{
            mu:    sync.Mutex{},
            store: make(map[string]Progress),
        }
    })
    return ups
}
```

**The `sync.Once` bug:** The first version declared `once` inside the constructor function — a new `sync.Once` every call. It had no guard effect. Moving it to package level fixed the issue.

### Why the Store Exists Before the Upload Starts

The three-endpoint flow has a race condition if not careful:

1. Client calls `POST /upload/initiate` → gets `upload_id`
2. Client opens SSE connection to `GET /upload/progress/{upload_id}`
3. Client starts `PUT /upload/{upload_id}`

If the progress store only has entries after `PUT` starts, the SSE handler (step 2) could open before the entry exists and return 404.

The fix: `initiate` creates the store entry immediately with an empty `Progress{}`. The SSE handler can connect at any time and will find the entry. The `SoFar == 0 && !IsComplete` check means it waits silently (2s sleep) until progress actually starts.

---

## Phase 5: The Token Bucket Rate Limiter

### Why Rate Limit?

`io.Copy` is fast. For a small test file, the entire upload completes in under a millisecond — no SSE events, instant completion. To observe the SSE progress stream meaningfully, the upload needs to be slow enough to emit multiple events.

The token bucket algorithm was the right approach: it limits read throughput while being mathematically principled about burst and refill.

### Token Bucket Mechanics

A token bucket has:
- **Balance** (`tokenBal`): tokens currently available
- **Refill rate** (`refillRate`): tokens added per second
- **Max burst** (`maxBurst`): capacity of the bucket

On each `Read(p []byte)`:
1. Calculate elapsed time since last read
2. Add `elapsed * refillRate` tokens to balance (capped at `maxBurst`)
3. Decide how many bytes to actually read based on current balance

### The Three Cases

This is the core insight. `len(p)` is the 32KB buffer that `io.Copy` passes in. `maxBurst` is configured at 20 bytes. These are very different scales.

**Case 1: `bal >= len(p)`**
We have enough tokens for the full read. Read `len(p)`, deduct from balance.

**Case 2: `0 < bal < len(p)`**
We have some tokens but fewer than requested. Allocate a temp buffer of size `bal`, read into that, copy to `p[:n]`, deduct from balance.

```go
temp := make([]byte, bal)
n, err := s.r.Read(temp)
copy(p, temp[:n])
```

**Case 3: `bal == 0`**
No tokens. Sleep until enough tokens accumulate for the next read:

```go
remaining := min(int64(len(p)), s.maxBurst)
waitTime := float64(remaining) / float64(s.refillRate)
time.Sleep(time.Duration(waitTime * float64(time.Second)))
```

Wait only until `min(len(p), maxBurst)` tokens accumulate — not a full refill. Then read.

### The 327-Second Sleep Bug

The first version had a single code path: calculate wait time as `(len(p) - bal) / refillRate`. With `len(p) = 32768` (32KB from io.Copy) and `refillRate = 100`, that's `32768 / 100 = 327 seconds` per read.

The bug was using `len(p)` as the target when the bucket can only ever hold `maxBurst = 20` tokens. You can't fill a 20-liter bucket with 327 liters of tokens. The fix was structuring the three cases properly, and in the zero-balance case, waiting only until `min(len(p), maxBurst)` tokens are available.

### The Integer Division Bug

The wait time calculation originally used integer arithmetic:

```go
waitTime := remaining / s.refillRate  // both int64
```

`20 / 100 = 0` in integer division. Zero wait, zero throttling. The fix:

```go
waitTime := float64(remaining) / float64(s.refillRate)  // 0.2 seconds
```

A subtle bug that produced wrong behavior silently. No error, no panic — just no throttling.

### Verification

With `maxBurst = 20`, `refillRate = 100`, uploading an 838-byte file:

- Each read yields ~20 bytes
- Wait time per read: `20 / 100 = 0.2 seconds`
- Total reads: `838 / 20 ≈ 42`
- Expected time: `42 × 0.2 = 8.4 seconds`
- Actual time: **8.236 seconds** ✓

---

## Phase 6: The `uploadwriter` — Progress-Tracking `io.Writer`

The progress store needs to be updated as bytes flow through `io.Copy`. But `io.Copy` takes `src io.Reader` and `dst io.Writer` — there's no hook.

The solution is the decorator/wrapper pattern: implement `io.Writer`, delegate to the real writer, update the store on each write.

```go
type Writer struct {
    w     io.Writer
    id    string
    sofar uint64
    ups   *uploadprogress.Store
}

func (uw *Writer) Write(p []byte) (int, error) {
    n, err := uw.w.Write(p)
    uw.sofar += uint64(n)
    uw.ups.SetProgress(uw.id, uploadprogress.Progress{
        Err:   err,
        SoFar: uw.sofar,
    })
    return n, err
}
```

The `io.Copy` call becomes:

```go
wr := uploadwriter.New(tmp, uploadid, u.ups)
n, err := io.Copy(wr, slowreader.New(part))
```

Every 32KB chunk that passes through triggers a progress update. The SSE handler on the other end polls every 200ms and sees the byte count climbing.

**The double-counting bug:** An earlier version had `SoFar: uw.sofar + uint64(n)` — incrementing `sofar` and then adding `n` again in the struct literal. Fix: increment first, then use `sofar` directly.

---

## Phase 7: SSE Progress Streaming

### Why SSE and Not WebSockets?

WebSockets are bidirectional. Upload progress is unidirectional — server pushes to client. SSE is the right tool: simpler protocol, built on HTTP, native browser support via `EventSource`.

SSE wire format:

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
Transfer-Encoding: chunked

event: upload-progress
data: {"progress": "180", "complete": false}

event: upload-progress
data: {"progress": "380", "complete": false}

event: upload-progress
data: {"progress": "838", "complete": true}

```

Every event is: `event: name\n`, `data: value\n`, then a blank line `\n`. The double newline terminates the event.

**The backtick bug:** The first version used backtick strings for the `fmt.Fprintf` format:

```go
fmt.Fprintf(w, `event: upload-progress\ndata: {...}\n\n`, ...)
```

Backtick strings are raw string literals in Go — `\n` is a literal backslash-n, not a newline. The SSE events never terminated correctly. Fix: use double-quoted strings.

### Why `Flush()` Is Required

Go's `http.ResponseWriter` buffers writes internally for performance. Without `Flush()`, the server accumulates multiple writes and sends them together — the client doesn't see individual events. For SSE, every event must be flushed immediately:

```go
fmt.Fprintf(w, "event: upload-progress\ndata: {...}\n\n", ...)
w.(http.Flusher).Flush()
```

This is also why `Transfer-Encoding: chunked` appears in the response — Go's `net/http` automatically uses chunked encoding when the response length is unknown (i.e., streaming).

### The Two-Connection Model

HTTP/1.1 is half-duplex on a single connection. A connection in the middle of a PUT request body can't simultaneously receive SSE events. The solution: two separate connections.

- **Connection 1:** PUT `/upload/{id}` — uploads the file (long-running, body streaming)
- **Connection 2:** GET `/upload/progress/{id}` — receives SSE events (long-running, response streaming)

Both connections are open simultaneously. The progress store is the shared state between the two concurrent operations.

### The SSE Handler Logic

```go
for {
    select {
    case <-r.Context().Done():
        return nil  // client disconnected
    default:
        progress, err := u.ups.GetProgressById(id)
        if err != nil {
            return apis.NewError(http.StatusNotFound, "upload-id not found")
        }

        if progress.IsComplete {
            // send final event, clean up store
            fmt.Fprintf(w, "event: ...\n\n", progress.SoFar, true)
            w.(http.Flusher).Flush()
            u.ups.DeleteProgressById(id)
            return nil
        }

        if progress.SoFar == 0 && !progress.IsComplete {
            time.Sleep(2000 * time.Millisecond)  // upload hasn't started yet
            continue
        }

        if progress.Err != nil && !progress.IsComplete {
            // send error event
            fmt.Fprintf(w, "event: ...\n\n", "failed to upload. Try again")
            w.(http.Flusher).Flush()
            return nil
        }

        fmt.Fprintf(w, "event: upload-progress\ndata: {\"progress\": \"%v\", \"complete\": %v}\n\n", progress.SoFar, progress.IsComplete)
        w.(http.Flusher).Flush()
        time.Sleep(200 * time.Millisecond)
    }
}
```

**Critical ordering:** Check `IsComplete` BEFORE checking `SoFar == 0`. If you check `SoFar == 0` first and the upload completes with `SoFar` still 0 for some reason, you'd loop forever. The `IsComplete` flag is the authoritative terminal state.

### The `IsComplete` Bug

After `io.Copy` finishes, the upload handler calls:

```go
u.ups.SetProgress(uploadid, uploadprogress.Progress{
    Err:        err,
    Total:      uint64(n),
    SoFar:      uint64(n),
    IsComplete: true,
})
```

An early version had `SoFar` missing (defaulting to 0) in the final `SetProgress`. The SSE handler saw `IsComplete = true` but `SoFar = 0`. The `SoFar == 0 && !IsComplete` check saved it from looping, but the final event sent `progress: 0` — wrong. Fix: always set `SoFar: uint64(n)` in the final progress update.

### The Path Parameter Bug

The route pattern was `GET /upload/progress/{uploadid}` but the handler used `r.PathValue("upload-id")` (with a hyphen). `PathValue` returns `""` for unrecognized names. The SSE handler was looking up an empty string in the store, getting a not-found error, and returning 404 immediately.

Fix: consistent naming. Route pattern `{uploadid}`, handler `r.PathValue("uploadid")`.

---

## Phase 8: Dependency Architecture

### The Circular Import Problem

Early structure had `internal/apis` importing a handler type from the upload package, and the upload package importing `internal/apis`. Go doesn't allow circular imports.

The solution: invert the dependency. The upload package registers its own routes. The `server.New()` function accepts an `http.Handler` — it doesn't know anything about specific routes. Routes are set up in `prototypes/router/` and the resulting handler is passed into `server.New()`.

```
cmd/prototypes/main.go
  → prototypes/router/router.go  (registers upload routes)
  → internal/server/server.go    (accepts http.Handler, knows nothing about routes)
```

Internal packages (`mux`, `apis`, `server`) have no imports from `prototypes/`. The dependency graph is strictly one-way.

### Sub-Package Structure

The upload handler is split into focused files rather than a single large file:

```
prototypes/01-file-upload/upload/
  upload.go                  → UploadAPI struct and constructor
  routes.go                  → Route registration
  route-initiate-upload.go   → POST /upload/initiate
  route-upload-file.go       → PUT /upload/{uploadid}
  route-upload-progress.go   → GET /upload/progress/{uploadid}
  uploadprogress/            → Progress store sub-package
  uploadwriter/              → Progress-tracking writer sub-package
  logs/upload.log            → Log samples for article
```

The sub-packages (`uploadprogress`, `uploadwriter`) are prototype-level details — they're not reusable across prototypes. This is intentional: different prototypes may have fundamentally different progress models.

---

## Phase 9: Observability

### Structured Logging with Trace IDs

Every log entry carries a trace ID that was generated at request start by the tracing middleware:

```json
{
  "time": "2026-04-18T07:38:26.123Z",
  "level": "INFO",
  "source": {"function": "upload.UploadFile", "file": "route-upload-file.go", "line": 42},
  "msg": "Request ended",
  "duration": "8.236s",
  "status": 200,
  "trace": {
    "trace_id": "fc89bb89-0cfb-4cd2-ba8a-9c49bcafa4a1",
    "request_method": "PUT",
    "request_endpoint": "/upload/fc89bb89-0cfb-4cd2-ba8a-9c49bcafa4a1"
  }
}
```

The trace ID is propagated through `context.Context`. Any log call anywhere in the call stack (handler, store, writer) automatically includes the trace ID.

**Flat log fields preference:** The trace struct is included as a nested object `"trace": {...}` rather than flat fields — a deliberate choice for log aggregator compatibility. Individual trace fields are under the `trace` key rather than at the top level of the JSON.

### Logger Implementation

The custom logger wraps `log/slog`. It extracts the call site via `runtime.Callers` and attaches the trace from context:

```go
func (l *Logger) write(ctx context.Context, level level, skipPc int, msg string, attr ...any) {
    pcs := make([]uintptr, 1)
    runtime.Callers(skipPc, pcs[:])
    r := slog.NewRecord(time.Now(), slog.Level(level), msg, pcs[0])
    r.Add(attr...)
    traceId, err := tracing.GetTrace(ctx)
    if err == nil {
        r.Add("trace", traceId)
    }
    l.h.Handler.Handle(ctx, r)
}
```

**The `pcs` panic:** The first version used `pcs := []uintptr{}` — a zero-length slice. `pcs[0]` panicked at runtime. Fix: `pcs := make([]uintptr, 1)`.

---

## The Complete Bug List

Every bug encountered, in roughly the order they appeared:

| Bug | Symptom | Root Cause | Fix |
|-----|---------|------------|-----|
| `http.DetectContentType` returns `text/plain` | Wrong content type validation | Body sniffing doesn't know multipart | Read `Content-Type` header, parse with `mime.ParseMediaType` |
| `content-desposition` typo | Every request rejected | Silent empty string from `Header.Get` | Correct spelling |
| Missing `return` after error | Double `WriteHeader` warnings | Execution fell through to next block | Add `return` after every error response |
| `WriteHeader` before `Header().Set` | Headers dropped silently | Headers must be set before status | Set headers first |
| Boundary check before mtype check | Misleading error messages | Wrong check order | Check mtype first, then boundary |
| Circular dependency | Build failure | `internal/apis` ↔ `upload` | Move routing to `prototypes/router/` |
| `sync.Once` inside function | No singleton guard | New `once` per call | Move to package level |
| `copy(temp, p)` wrong direction | Corrupted reads | Args reversed | `copy(p, temp[:n])` |
| Integer division in wait time | No throttling | `20/100 = 0` | Use `float64` division |
| 327-second sleep | Upload hangs forever | `len(p) / refillRate` ignores maxBurst | Three-case structure, wait for `min(len(p), maxBurst)` tokens |
| Recursive `WriteHeader` | Stack overflow | `w.WriteHeader` called itself | Change to `w.w.WriteHeader` |
| Missing `Flusher` on response writer | SSE panic | Wrapper didn't implement `http.Flusher` | Add `Flush()` method |
| Backtick string `\n` | SSE events never terminate | Raw string literal, not newline | Use double-quoted strings |
| `pcs := []uintptr{}` | Logger panic | Zero-length slice, `pcs[0]` out of bounds | `make([]uintptr, 1)` |
| `SoFar == 0` check before `IsComplete` | SSE loops forever after completion | Wrong condition order | Check `IsComplete` first |
| Final `SetProgress` missing `SoFar` | Progress shows 0 at completion | Field not set in final update | Set `SoFar: uint64(n)` |
| `PathValue("upload-id")` mismatch | SSE returns 404 immediately | Hyphen in name doesn't match route pattern | Consistent `{uploadid}` / `r.PathValue("uploadid")` |
| `uploadwriter` double counting | Progress overcounts | `sofar + n` after already incrementing `sofar` | Use `sofar` after increment |
| `copy(p, temp)` full copy | Potentially incorrect data | Should copy only filled bytes | `copy(p, temp[:n])` |
| SSE `status: 0` in logs | Tracing logs wrong status for SSE requests | Implicit 200 bypasses `WriteHeader`, `StatusCode` stays 0 | Capture in `Write`: if `StatusCode == 0`, set to 200 before delegating |

---

## Key Protocol Mental Models

### Memory Flow for an 8GB Upload

```
Internet
  → NIC (hardware)
  → DMA ring buffer (kernel, zero-copy)
  → TCP receive buffer (kernel, ~128KB–256KB)
  → io.Copy 32KB buffer (user-space, reused)
  → os.File write (user-space, page cache)
  → disk

Peak RAM usage: ~160KB regardless of file size
```

### SSE Wire Format

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Transfer-Encoding: chunked    ← automatically applied by net/http

[chunk size in hex]\r\n
event: upload-progress\r\n
data: {"progress": "838", "complete": true}\r\n
\r\n
[chunk size in hex]\r\n
\r\n
0\r\n                          ← terminal empty chunk
\r\n
```

The blank line between events is the SSE protocol's event delimiter. `Flush()` forces each event to be sent as its own chunk rather than accumulating in the response buffer.

### Token Bucket

```
Tokens = currency
Read(p) = purchase

If you have enough tokens: read, deduct, return
If you have some tokens: read less (bal bytes), deduct, return
If you have no tokens: sleep until tokens accumulate, then read

Refill happens continuously based on elapsed time, not on a timer goroutine
```

### HTTP/1.1 Half-Duplex Constraint

A single HTTP/1.1 connection is half-duplex — request body flows in one direction, response in the other, but not simultaneously in both. A connection busy receiving a large PUT body can't also send SSE events. This is why upload + progress requires two connections.

HTTP/2 multiplexing would let both streams share a single connection (different stream IDs). This prototype uses HTTP/1.1 as the baseline.

### Middleware Ordering

```
mux.HandleFunc("/resource", handler, m1, m2)

Execution order: m2 → m1 → handler
(rightmost = most qualified = runs last before handler)

Like opening nested boxes:
outer box (m2) → inner box (m1) → handler
```

---

## Verified Output

Working SSE stream for an 838-byte file at 100 bytes/sec throttle:

```
event: upload-progress
data: {"progress": "180", "complete": false}

event: upload-progress
data: {"progress": "360", "complete": false}

event: upload-progress
data: {"progress": "380", "complete": false}

event: upload-progress
data: {"progress": "560", "complete": false}

event: upload-progress
data: {"progress": "580", "complete": false}

event: upload-progress
data: {"progress": "760", "complete": false}

event: upload-progress
data: {"progress": "780", "complete": false}

event: upload-progress
data: {"progress": "838", "complete": true}
```

Total time: 8.236 seconds. Expected: ~8.4 seconds. The token bucket works.

---

## Final Architecture

```
internal/
  mux/
    mux.go              → Custom mux with error-returning handler type
    middlewares.go      → Middleware chain (reversed iteration)
  middlewares/
    tracing/
      tracing.go        → Request trace ID, start/end logging
      responseWriter.go → MuxResponseWriter (captures status, implements Flusher)
    errors/
      errors.go         → Typed error handler → JSON response
  apis/
    errors.go           → apis.Error type (status code + message)
    response.go         → WriteJson helper
  tracing/
    context.go          → Trace struct, WithTrace/GetTrace
  utilities/
    logger/
      logger.go         → Custom slog wrapper with trace propagation
      handler.go        → slog Handler implementation
  ratelimit/
    slowreader/
      slowreader.go     → Token bucket rate limiter (io.Reader wrapper)
  server/
    server.go           → HTTP server setup, accepts http.Handler

prototypes/
  router/
    router.go           → Route registration (avoids circular imports)
  01-file-upload/
    upload/
      upload.go         → UploadAPI struct
      routes.go         → Route wiring
      route-initiate-upload.go → POST /upload/initiate
      route-upload-file.go     → PUT /upload/{uploadid}
      route-upload-progress.go → GET /upload/progress/{uploadid}
      uploadprogress/
        uploadprogress.go → Singleton progress store (mutex-protected map)
      uploadwriter/
        uploadwriter.go   → io.Writer wrapper for progress tracking
      logs/
        upload.log        → Sample log output for article

cmd/prototypes/
  main.go               → Entry point
```

---

## What This Prototype Teaches

1. **HTTP headers are declarative.** The client declares content type in headers. Don't sniff the body.

2. **Streaming means never accumulating.** `r.MultipartReader()` + `io.Copy` + `io.Writer` chain keeps RAM at ~160KB for any file size.

3. **Go's `io.Reader` contract is precise.** Caller allocates, reader fills, only `p[:n]` is valid. Everything in the standard library is built on this.

4. **Middleware is just function composition.** `MiddlewareFunc func(Handler) Handler` — every middleware is a function that returns a function. Composition order is a design choice that must be documented.

5. **Response writer wrappers must implement all interfaces.** If a handler downstream needs `http.Flusher`, the wrapper must implement it. Silent type assertion panics at runtime.

6. **SSE is HTTP with `text/event-stream` + `Transfer-Encoding: chunked` + `Flush()`.** No websocket upgrade. No special protocol. Just a long-lived GET with writes flushed immediately.

7. **Rate limiting is three cases, not one.** Token bucket `Read` splits into: enough tokens, some tokens, no tokens. Each case has different behavior.

8. **Precision matters.** `20/100 = 0` (integer). `20.0/100.0 = 0.2` (float). A single type mismatch makes throttling disappear silently.

9. **Shared state requires a pre-agreed key.** Upload ID must exist in the store before the SSE handler connects. The `initiate` endpoint solves this race by registering the ID before either connection starts uploading or listening.

10. **Circular imports reveal architectural problems.** The fix isn't a workaround — it's identifying which package should own each responsibility.

---

## Next: Prototype 02

HTTP Client Connection Pool Analyzer — visualize connection reuse, test pool configurations, ASCII timeline charts, and tuning recommendations.
