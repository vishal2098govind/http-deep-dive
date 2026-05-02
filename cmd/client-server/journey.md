# Journey Document: TCP Echo Server — From Availability to Protocol Design

> This document is a structured brain dump to be shaped into a Substack article.
> It continues from the previous article: ["I Thought Go Concurrency Was About Speed — I Was Wrong"](https://vishalageek.substack.com/p/i-thought-go-concurrency-was-about) — published in The Engineer's Notebook.

---

## Thread Back to Article 1

The previous article — ["I Thought Go Concurrency Was About Speed — I Was Wrong"](https://vishalageek.substack.com/p/i-thought-go-concurrency-was-about) — walked through evolving a raw TCP server in Go across four stages:

1. **Single blocking Accept()** — server stuck after one client connected
2. **Reading from connection** — server did something useful, but still single-client
3. **Loop with serial handling** — accepted multiple connections, but one at a time
4. **Goroutine per connection** — the breakthrough: clients stopped blocking each other

The key realization: concurrency made the server *available*, not *faster*. The mental shift was from "how do I make this concurrent?" to "what is blocking, and who does it make unavailable?"

The article ended with a note that the server was still incomplete — no coordination between clients, no graceful shutdown, no understanding of what actually happens inside that goroutine.

**That open question is what this journey picks up.**

What actually happens inside that goroutine? What does a TCP connection mean at the kernel level in Go?

---

## What I Wanted to Understand

During BTech Computer Science, I studied TCP — the 3-way handshake (SYN, SYN-ACK, ACK) and the 4-way teardown (FIN, ACK, FIN, ACK). It was theoretical. Textbook diagrams, sequence numbers, state machines.

What I never understood:
- What does a TCP connection look like as a kernel data structure?
- How does Go's runtime handle millions of goroutines without millions of OS threads?
- What is Go's netpoller, and why does it matter?
- What actually happens when you call `conn.Read()` and there's no data yet?

I wanted to see this at the wire and kernel level — not in theory, but through code I wrote and understood line by line.

---

## What I Built

I built a raw TCP echo server and client in Go — no `net/http`, no frameworks. Just `net.Dial`, `net.Listen`, `net.Conn`.

**The server:**
- Listens on `:4040`
- Accepts connections in an infinite loop
- Spawns a goroutine per connection
- Reads lines via `bufio.Scanner`
- Echoes each line back to the client

**The client:**
- Connects to `:4040`
- Reads from `os.Stdin` and writes to conn (writer goroutine)
- Reads from conn and writes to `os.Stdout` (reader goroutine)
- Uses a channel to keep `main` alive until the server closes the connection

---

## The TCP Connection — What I Drew Out

I drew an extensive diagram (excalidraw) mapping the full lifecycle of a TCP connection in Go. Here's what it covers:

### Server Side — Connection Establishment

1. `net.Listen("tcp", ":4040")` triggers three syscalls:
   - `socket()` — creates a TCP socket on kernel RAM, assigns file descriptor (e.g. `fd=5`)
   - `bind(fd, ":4040")` — checks if port is in use, checks permissions, registers in kernel's port-to-socket table
   - `listen(fd)` — transitions socket to LISTEN state, creates two kernel queues: **SYN queue** and **accept queue**

2. When a SYN packet arrives from a client:
   - Kernel creates a new TCP socket for this client (e.g. `fd=10`)
   - Adds client to SYN queue
   - Sends SYN-ACK
   - On ACK receipt: moves client to accept queue, marks socket as ESTABLISHED
   - **The goroutine is not involved in any of this — the kernel handles the handshake independently**

3. `listener.Accept()` — the goroutine asks Go runtime for the next connection from the accept queue. If the queue is empty, the goroutine is **parked** (removed from OS thread).

### Go's Netpoller — Why Goroutines Are Cheap

This is the core insight. Go doesn't block OS threads on I/O. Instead:

- When a goroutine calls `Accept()` or `conn.Read()` and there's nothing ready, Go calls `gopark()` — the goroutine is suspended and the OS thread is freed for other work.
- Go registers the file descriptor with **epoll** via `epoll_ctl(ADD, fd, EPOLLIN)`.
- The netpoller (a separate OS thread) runs `epoll_wait()` in a loop.
- When the kernel signals data is ready (e.g. client sent bytes, accept queue has a connection), `epoll_wait` returns the ready fd.
- The netpoller finds the parked goroutine via a `pollDesc` struct `{fd, rg *g, wg *g}` and puts it back on the run queue.
- The goroutine resumes exactly where it was parked.

**This is why Go can handle millions of concurrent connections with a small thread pool. Goroutines are cheap because they don't hold OS threads while waiting for I/O.**

### Client Side — Connection Establishment

- `net.Dial("tcp", ":4040")` triggers `socket()` + `connect(fd, ":4040")`
- Kernel assigns an ephemeral port (32768–60999) automatically
- `connect()` returns `EINPROGRESS` immediately — handshake is in progress
- Go registers `fd` with epoll for `EPOLLOUT` (writable = connected)
- Client goroutine is parked via `gopark()`
- On SYN-ACK receipt, kernel completes handshake, epoll fires, netpoller unparks goroutine
- `net.Dial` returns `conn (fd=3)`, socket is now ESTABLISHED

---

## The Echo Server — Mistakes and Lessons

### Bidirectional communication needs two goroutines

`io.Copy(conn, os.Stdin)` is one direction. For echo, the client also needs to receive. Two goroutines:

```
writer goroutine: os.Stdin → conn
reader goroutine: conn → os.Stdout
```

If you run them sequentially, the first blocks forever. They must be concurrent.

### Main goroutine exits too early

With both goroutines running, `main` exits immediately. Used a channel:

```go
done := make(chan any, 1)
// reader goroutine signals done after io.Copy returns
done <- 1
// main waits
<-done
```

**Why the reader, not the writer?** The reader finishing means the server closed its side — nothing more is coming. That's the true end of the conversation. The writer might finish (user typed sentinel) while the server is still sending the last echo back.

### Custom sentinel protocol — "EOF" as termination signal

Wanted a way to close the connection from the terminal without Ctrl+D (actual stdin EOF). Built a `WrappedReader` around `os.Stdin`:

- Implements `io.Reader`
- Reads from stdin first, then inspects what was read
- If the line is `"EOF\n"`, returns `(0, io.EOF)` to the caller (`io.Copy`)
- Otherwise passes bytes through normally

Key learnings from building this:
- `p` in `Read(p []byte)` is the caller's empty buffer — you must read into it first, then inspect `p[:n]`
- Terminal stdin is line-buffered — you get `"EOF\n"` as one chunk, not byte by byte
- `string(p)` is wrong; `string(p[:n])` is right — `p` is 32KB, only `n` bytes are valid

### Half-close with CloseWrite()

After the writer goroutine's `io.Copy` returns (sentinel detected), the connection needs to be half-closed — write side only. The client is still reading echoes from the server.

- `conn.Close()` — closes both directions. Reader goroutine would break.
- `conn.(*net.TCPConn).CloseWrite()` — sends FIN on write side only. Server's `bufio.Scanner` sees EOF, exits the scan loop, closes conn. Client's reader goroutine sees conn close, `io.Copy` returns, `done <- 1` fires.

**Type assertion needed:** `net.Conn` doesn't expose `CloseWrite()`. Must assert to `*net.TCPConn`.

### Server must close conn after scan loop

The server's goroutine must call `conn.Close()` (or `defer conn.Close()`) after the scanner loop exits. Without it, the server goroutine returns but the conn is not closed at the kernel level — client's reader blocks forever waiting for data that never comes.

`defer conn.Close()` at the top of the goroutine covers all exit paths cleanly.

---

## What "EOF" as a Protocol Taught Me

The `"EOF\n"` sentinel is a tiny custom protocol. Both client and server agreed:

> "If you see this string, the conversation is over."

This surfaces the real problem with text-based in-band signaling:

- What if the user actually wants to send the literal string `"EOF"`? The protocol can't represent it — it gets misinterpreted as a control signal.
- What if the payload is binary (a JPEG byte stream)? The sentinel approach breaks completely — any byte sequence could accidentally match.

This is exactly the problem real protocols solve:
- **HTTP** uses `Content-Length` or `Transfer-Encoding: chunked` for framing
- **RESP** (Redis) uses type-tagged, length-prefixed messages (`$6\r\nfoobar\r\n`)
- **gRPC** uses binary length-prefixed frames over HTTP/2

The next natural step: design a framing protocol that is binary-safe and doesn't rely on in-band control strings.

---

## Key Mental Models

### Why Go handles millions of connections
```
goroutine parks → OS thread freed → netpoller watches fd via epoll
kernel signals ready → netpoller unparks goroutine → resumes on any OS thread
```
No thread-per-connection. No blocking threads. Just goroutines sleeping in the scheduler.

### Connection teardown flow (with CloseWrite)
```
Client types "EOF\n"
→ WrappedReader returns io.EOF
→ writer goroutine's io.Copy returns
→ conn.CloseWrite() sends FIN to server
→ server's bufio.Scanner sees EOF, loop exits
→ server's defer conn.Close() fires, sends FIN to client
→ client's reader io.Copy returns
→ done <- 1
→ main exits, conn.Close() called
```

### The sentinel protocol limitation
```
Text sentinel "EOF\n" → breaks on binary data
Length-prefix framing → binary-safe, no ambiguity
Real protocols (HTTP, RESP, gRPC) use framing, not sentinels
```

---

## Where This Goes Next

- **Connection termination in depth** — 4-way FIN teardown at the kernel level, TIME_WAIT state, why it exists
- **Data transfer phase** — send_buff, rcv_buff, TCP flow control, backpressure
- **Protocol framing** — designing a binary-safe framing layer, leading toward understanding RESP/HTTP framing
- **Connection pooling** — why opening a new TCP connection for every request is expensive (handshake cost, kernel resource allocation), how pools reuse established connections, what happens to idle connections, and how this ties back to the netpoller and goroutine lifecycle
- **Eventual goal** — understand HTTP as a protocol built on top of exactly these primitives: framing + connection pooling + request/response semantics

---

## Tone Reference

Previous article style: first-person, conversational, realization-driven. Each section surfaces a specific moment of confusion that resolved into clarity. Not a tutorial — a learning journal.

Target reader: backend engineers who use Go but haven't looked beneath the abstractions.
