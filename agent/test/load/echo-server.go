// Echo server for the gRPC tunnel load harness.
//
// Deterministic by construction so load generators can validate byte
// integrity end to end:
//
//	ANY /echo/{id}?delay_ms=D&size=S&status=C&seed=X
//	  - sleeps D ms
//	  - returns status C (default 200)
//	  - body: S bytes generated from seed X with the shared PRNG below
//	    (the load generator regenerates the same stream and compares)
//	  - response headers echo what was received:
//	      x-echo-request-sha256: hex sha256 of the request body
//	      x-echo-bytes-received: request body length
//	      x-echo-request-id:     the {id} path segment
//	      x-echo-injected:       value of the x-load-injected request
//	                             header (accept-file rule injection check)
//	GET /healthz
//
// Run with: go run echo-server.go  (stdlib only, no module required)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// prngFill fills buf with a deterministic byte stream derived from seed
// using xorshift64*. Keep bit-for-bit identical to the copy in loadgen.go.
func prngFill(buf []byte, seed uint64) {
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	x := seed
	for i := 0; i < len(buf); i += 8 {
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		v := x * 0x2545F4914F6CDD1D
		for j := 0; j < 8 && i+j < len(buf); j++ {
			buf[i+j] = byte(v >> (8 * j))
		}
	}
}

// echoName identifies this echo-server instance. Each broker token in the
// load test points at its own instance, and the load generator asserts the
// response came back from the right one — that assertion is what proves
// requests never leak between logically separate token pools.
var echoName = "echo"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if n := os.Getenv("ECHO_NAME"); n != "" {
		echoName = n
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/echo/", handleEcho)

	log.Printf("echo-server %q listening on :%s", echoName, port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handleEcho(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/echo/")
	q := r.URL.Query()

	delayMs := atoiDefault(q.Get("delay_ms"), 0)
	size := atoiDefault(q.Get("size"), 0)
	status := atoiDefault(q.Get("status"), http.StatusOK)
	seed, _ := strconv.ParseUint(q.Get("seed"), 10, 64)

	// Consume and hash the request body (streamed; no full buffering).
	h := sha256.New()
	n, err := io.Copy(h, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("body read failed: %v", err), http.StatusBadRequest)
		return
	}

	if delayMs > 0 {
		time.Sleep(time.Duration(delayMs) * time.Millisecond)
	}

	w.Header().Set("x-echo-request-sha256", hex.EncodeToString(h.Sum(nil)))
	w.Header().Set("x-echo-bytes-received", strconv.FormatInt(n, 10))
	w.Header().Set("x-echo-request-id", id)
	w.Header().Set("x-echo-injected", r.Header.Get("x-load-injected"))
	w.Header().Set("x-echo-server", echoName)
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.WriteHeader(status)

	// Stream the deterministic response body in chunks.
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	remaining := size
	// The PRNG is stateful across the whole body: regenerate the full
	// stream chunk by chunk with a rolling seed derived per chunk index so
	// both sides can compute it without holding the body in memory.
	idx := uint64(0)
	for remaining > 0 {
		c := chunk
		if remaining < c {
			c = remaining
		}
		prngFill(buf[:c], seed+idx)
		if _, err := w.Write(buf[:c]); err != nil {
			return // caller went away
		}
		remaining -= c
		idx++
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
