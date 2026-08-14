// Load generator for the gRPC tunnel load harness.
//
// Routes the way Cortex's dispatcher would: for each request it asks the
// dispatcher-mock which tunnel-server instances currently hold a stream
// for the target token, picks one, and POSTs through the broker ingress:
//
//	POST http://{server}:8080/broker/{token}/echo/{id}?delay_ms&size&status&seed
//
// Every request is fully validated: intended status code, echoed request
// id, sha256 of the uploaded body as computed by the echo server, the
// accept-file injected header, and the byte-exact (streamed, chunkwise)
// content of the deterministic response body.
//
// Failure taxonomy:
//   - integrity_failure: wrong bytes/headers on a completed exchange —
//     always fatal, tolerance zero.
//   - routing_retry: transport error or infra 5xx (a response WITHOUT the
//     echo server's headers); retried up to RETRIES times with
//     re-resolution. Expected during chaos.
//   - availability_failure: retries exhausted.
//
// Env: TOKENS (comma-separated raw broker tokens), DISPATCHER_URL,
// WORKERS, DURATION, MAX_BODY_BYTES, MAX_RESP_BYTES, MAX_DELAY_MS,
// MIN_SUCCESS_PCT, REPORT_PATH, READY_TIMEOUT.
//
// Run with: go run loadgen.go  (stdlib only)
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// prngFill fills buf with a deterministic byte stream derived from seed
// using xorshift64*. Keep bit-for-bit identical to the copy in
// echo-server.go.
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

const echoChunk = 64 * 1024 // must match echo-server.go's response chunking

type config struct {
	tokens        []string
	dispatcherURL string
	workers       int
	duration      time.Duration
	maxBody       int
	maxResp       int
	maxDelayMs    int
	minSuccessPct float64
	reportPath    string
	readyTimeout  time.Duration
	retries       int
	serverPort    string
	serverHost    string // override for non-docker smoke runs
}

type stats struct {
	mu                  sync.Mutex
	ok                  int
	integrityFailures   int
	availabilityFails   int
	routingRetries      int
	latenciesMs         []float64
	integrityDetails    []string
	availabilityDetails []string
}

func main() {
	cfg := loadConfig()
	hostname, _ := os.Hostname()
	log.Printf("loadgen %s starting: workers=%d duration=%v tokens=%v",
		hostname, cfg.workers, cfg.duration, cfg.tokens)

	client := &http.Client{Timeout: 2 * time.Minute}
	resolver := newResolver(cfg.dispatcherURL, client)

	if err := waitReady(cfg, resolver); err != nil {
		log.Fatalf("readiness wait failed: %v", err)
	}
	log.Printf("all tokens routable; starting load")

	st := &stats{}
	deadline := time.Now().Add(cfg.duration)
	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				runOne(cfg, client, resolver, st)
			}
		}(w)
	}
	wg.Wait()

	writeReport(cfg, hostname, st)
}

func loadConfig() config {
	cfg := config{
		tokens:        strings.Split(envDefault("TOKENS", "tok-a"), ","),
		dispatcherURL: envDefault("DISPATCHER_URL", "http://dispatcher-mock:8080"),
		workers:       atoiEnv("WORKERS", 16),
		duration:      durEnv("DURATION", 2*time.Minute),
		maxBody:       atoiEnv("MAX_BODY_BYTES", 2<<20),
		maxResp:       atoiEnv("MAX_RESP_BYTES", 4<<20),
		maxDelayMs:    atoiEnv("MAX_DELAY_MS", 1000),
		minSuccessPct: floatEnv("MIN_SUCCESS_PCT", 99.0),
		reportPath:    envDefault("REPORT_PATH", "/reports"),
		readyTimeout:  durEnv("READY_TIMEOUT", 2*time.Minute),
		retries:       atoiEnv("RETRIES", 3),
		serverPort:    envDefault("SERVER_PORT", "8080"),
		serverHost:    os.Getenv("SERVER_HOST_OVERRIDE"),
	}
	return cfg
}

// resolver queries the dispatcher-mock with a tiny cache so the mock isn't
// hammered once per request.
type resolver struct {
	base   string
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cachedServers
}

type cachedServers struct {
	servers []string
	at      time.Time
}

func newResolver(base string, client *http.Client) *resolver {
	return &resolver{base: base, client: client, cache: make(map[string]cachedServers)}
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func (r *resolver) servers(rawToken string, bypassCache bool) ([]string, error) {
	hashed := hashToken(rawToken)
	r.mu.Lock()
	if c, ok := r.cache[hashed]; ok && !bypassCache && time.Since(c.at) < 2*time.Second {
		s := c.servers
		r.mu.Unlock()
		return s, nil
	}
	r.mu.Unlock()

	resp, err := r.client.Get(r.base + "/servers/" + hashed)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Servers []string `json:"servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cache[hashed] = cachedServers{servers: out.Servers, at: time.Now()}
	r.mu.Unlock()
	return out.Servers, nil
}

func waitReady(cfg config, res *resolver) error {
	deadline := time.Now().Add(cfg.readyTimeout)
	for time.Now().Before(deadline) {
		allReady := true
		for _, tok := range cfg.tokens {
			servers, err := res.servers(tok, true)
			if err != nil || len(servers) == 0 {
				allReady = false
				break
			}
		}
		if allReady {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("tokens not all routable within %v", cfg.readyTimeout)
}

// pickSize biases toward small payloads with an occasional large one.
func pickSize(max int) int {
	if max <= 0 {
		return 0
	}
	r := mrand.Float64()
	switch {
	case r < 0.70:
		return mrand.IntN(min(4<<10, max) + 1)
	case r < 0.90:
		return mrand.IntN(min(256<<10, max) + 1)
	default:
		return mrand.IntN(max + 1)
	}
}

func pickDelayMs(max int) int {
	if max <= 0 {
		return 0
	}
	if mrand.Float64() < 0.70 {
		return mrand.IntN(min(50, max) + 1)
	}
	return mrand.IntN(max + 1)
}

// pickStatus returns the intended echo status. Only non-5xx values so any
// 5xx we receive is unambiguously infrastructure.
func pickStatus() int {
	r := mrand.Float64()
	switch {
	case r < 0.90:
		return 200
	case r < 0.95:
		return 404
	default:
		return 400
	}
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func runOne(cfg config, client *http.Client, res *resolver, st *stats) {
	token := cfg.tokens[mrand.IntN(len(cfg.tokens))]
	id := randomID()
	bodySize := pickSize(cfg.maxBody)
	respSize := pickSize(cfg.maxResp)
	delayMs := pickDelayMs(cfg.maxDelayMs)
	status := pickStatus()
	var seedBytes [8]byte
	rand.Read(seedBytes[:])
	seed := binary.LittleEndian.Uint64(seedBytes[:])

	// Deterministic request body + its hash.
	body := make([]byte, bodySize)
	prngFill(body, seed^0xABCDEF)
	bodyHash := sha256.Sum256(body)
	wantBodyHash := hex.EncodeToString(bodyHash[:])

	path := fmt.Sprintf("/echo/%s?delay_ms=%d&size=%d&status=%d&seed=%d",
		id, delayMs, respSize, status, seed)

	start := time.Now()
	for attempt := 0; ; attempt++ {
		servers, err := res.servers(token, attempt > 0)
		if err != nil || len(servers) == 0 {
			if attempt >= cfg.retries {
				st.availability(fmt.Sprintf("no routable server for %s: %v", token, err))
				return
			}
			st.retry()
			time.Sleep(250 * time.Millisecond)
			continue
		}
		server := servers[mrand.IntN(len(servers))]
		if cfg.serverHost != "" {
			server = cfg.serverHost
		}
		url := fmt.Sprintf("http://%s:%s/broker/%s%s", server, cfg.serverPort, token, path)

		outcome, detail := attemptOnce(client, url, id, status, respSize, seed, wantBodyHash, body)
		switch outcome {
		case "ok":
			st.success(time.Since(start))
			return
		case "integrity":
			st.integrity(detail + " url=" + url)
			return
		case "infra":
			if attempt >= cfg.retries {
				st.availability(detail + " url=" + url)
				return
			}
			st.retry()
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// attemptOnce performs one exchange. Returns outcome ∈ {ok, integrity,
// infra} and a detail string.
func attemptOnce(client *http.Client, url, id string, wantStatus, respSize int, seed uint64, wantBodyHash string, body []byte) (string, string) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "infra", fmt.Sprintf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return "infra", fmt.Sprintf("transport: %v", err)
	}
	defer resp.Body.Close()

	// A response without the echo server's request-id header did not come
	// from the echo server — it's tunnel infrastructure (no-tunnel 502,
	// dispatch timeout 504, adapter error...). Retryable.
	if resp.Header.Get("x-echo-request-id") == "" {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "infra", fmt.Sprintf("infra status %d: %s", resp.StatusCode, string(snippet))
	}

	// From here on, everything is an end-to-end integrity property.
	if resp.StatusCode != wantStatus {
		return "integrity", fmt.Sprintf("status: want %d got %d", wantStatus, resp.StatusCode)
	}
	if got := resp.Header.Get("x-echo-request-id"); got != id {
		return "integrity", fmt.Sprintf("request id: want %s got %s", id, got)
	}
	if got := resp.Header.Get("x-echo-request-sha256"); got != wantBodyHash {
		return "integrity", fmt.Sprintf("request body hash: want %s got %s (sent %d bytes, echo saw %s)",
			wantBodyHash, got, len(body), resp.Header.Get("x-echo-bytes-received"))
	}
	if got := resp.Header.Get("x-echo-injected"); got != "yes" {
		return "integrity", fmt.Sprintf("accept-file injected header: want yes got %q", got)
	}

	// Stream-validate the deterministic response body chunk by chunk,
	// mirroring the echo server's generation exactly.
	want := make([]byte, echoChunk)
	got := make([]byte, echoChunk)
	remaining := respSize
	idx := uint64(0)
	for remaining > 0 {
		c := echoChunk
		if remaining < c {
			c = remaining
		}
		if _, err := io.ReadFull(resp.Body, got[:c]); err != nil {
			return "infra", fmt.Sprintf("response truncated with %d bytes left: %v", remaining, err)
		}
		prngFill(want[:c], seed+idx)
		if !bytes.Equal(got[:c], want[:c]) {
			return "integrity", fmt.Sprintf("response corruption in chunk %d (size %d)", idx, respSize)
		}
		remaining -= c
		idx++
	}
	if n, _ := io.Copy(io.Discard, resp.Body); n > 0 {
		return "integrity", fmt.Sprintf("response longer than expected by %d bytes", n)
	}

	return "ok", ""
}

func (st *stats) success(d time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ok++
	st.latenciesMs = append(st.latenciesMs, float64(d.Milliseconds()))
}

func (st *stats) retry() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.routingRetries++
}

func (st *stats) integrity(detail string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.integrityFailures++
	if len(st.integrityDetails) < 20 {
		st.integrityDetails = append(st.integrityDetails, detail)
	}
	log.Printf("INTEGRITY FAILURE: %s", detail)
}

func (st *stats) availability(detail string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.availabilityFails++
	if len(st.availabilityDetails) < 20 {
		st.availabilityDetails = append(st.availabilityDetails, detail)
	}
	log.Printf("availability failure: %s", detail)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)-1))
	return sorted[i]
}

func writeReport(cfg config, hostname string, st *stats) {
	st.mu.Lock()
	defer st.mu.Unlock()

	total := st.ok + st.availabilityFails + st.integrityFailures
	successPct := 100.0
	if total > 0 {
		successPct = float64(st.ok) / float64(total) * 100
	}
	sort.Float64s(st.latenciesMs)

	report := map[string]any{
		"host":                 hostname,
		"total":                total,
		"ok":                   st.ok,
		"integrity_failures":   st.integrityFailures,
		"availability_fails":   st.availabilityFails,
		"routing_retries":      st.routingRetries,
		"success_pct":          successPct,
		"latency_p50_ms":       percentile(st.latenciesMs, 50),
		"latency_p95_ms":       percentile(st.latenciesMs, 95),
		"latency_p99_ms":       percentile(st.latenciesMs, 99),
		"integrity_details":    st.integrityDetails,
		"availability_details": st.availabilityDetails,
	}

	pass := st.integrityFailures == 0 && successPct >= cfg.minSuccessPct && total > 0
	report["pass"] = pass

	data, _ := json.MarshalIndent(report, "", "  ")
	path := fmt.Sprintf("%s/loadgen-%s.json", cfg.reportPath, hostname)
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("failed to write report %s: %v", path, err)
	}
	log.Printf("REPORT %s", string(data))

	if !pass {
		log.Printf("FAIL: integrity=%d success=%.2f%% (min %.2f%%) total=%d",
			st.integrityFailures, successPct, cfg.minSuccessPct, total)
		os.Exit(1)
	}
	log.Printf("PASS: %d requests, %.2f%% success, %d routing retries",
		total, successPct, st.routingRetries)
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiEnv(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func floatEnv(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func durEnv(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
