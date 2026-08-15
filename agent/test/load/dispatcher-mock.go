// Dispatcher mock for the gRPC tunnel load harness.
//
// Plays the role Cortex's dispatcher plays in production: it receives the
// BROKER_SERVER notifications the tunnel servers already send (see
// server/broker/broker_server_client.go for the client side) and answers
// "which server instances currently hold a stream for this token" so load
// generators route requests the way Cortex would.
//
// Notification surface (tunnel server → mock):
//
//	POST   /internal/brokerservers/{serverId}                       server starting
//	DELETE /internal/brokerservers/{serverId}                       server stopping
//	POST   /internal/brokerservers/{serverId}/connections/{token}   client connected (refreshed periodically)
//	DELETE /internal/brokerservers/{serverId}/connections/{token}   client disconnected
//
// Query surface (load generator / orchestrator → mock):
//
//	GET /servers/{hashedToken}  → {"servers":["<serverId>", ...]}   fresh + live only
//	GET /state                  → full dump for validation
//	GET /probe                  → fetches /healthz from every live server
//	                              (they're only resolvable inside the
//	                              compose network) and returns the results
//	GET /healthz
//
// Freshness: the tunnel server re-sends client-connected for every token
// every RE_REGISTRATION_INTERVAL; entries older than STALE_AFTER (default
// 45s) are excluded from /servers answers and flagged in /state.
//
// Run with: go run dispatcher-mock.go  (stdlib only)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type connKey struct {
	ServerID string
	Token    string // hashed token as sent by the tunnel server
}

type event struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	ServerID string    `json:"serverId"`
	Token    string    `json:"token,omitempty"`
}

type mock struct {
	mu          sync.Mutex
	liveServers map[string]time.Time // serverId -> since
	conns       map[connKey]time.Time
	events      []event
	staleAfter  time.Duration
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	staleAfter := 45 * time.Second
	if v := os.Getenv("STALE_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("invalid STALE_AFTER: %v", err)
		}
		staleAfter = d
	}

	m := &mock{
		liveServers: make(map[string]time.Time),
		conns:       make(map[connKey]time.Time),
		staleAfter:  staleAfter,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/internal/brokerservers/", m.handleNotification)
	mux.HandleFunc("/servers/", m.handleServersQuery)
	mux.HandleFunc("/state", m.handleState)
	mux.HandleFunc("/probe", m.handleProbe)

	log.Printf("dispatcher-mock listening on :%s (staleAfter=%v)", port, staleAfter)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func (m *mock) record(kind, serverID, token string) {
	m.events = append(m.events, event{At: time.Now(), Kind: kind, ServerID: serverID, Token: token})
	log.Printf("%s server=%s token=%s", kind, serverID, token)
}

// handleNotification handles the BROKER_SERVER dispatcher API surface.
// Paths: /internal/brokerservers/{serverId}[/connections/{hashedToken}]
func (m *mock) handleNotification(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body) // accept and discard jsonapi bodies

	rest := strings.TrimPrefix(r.URL.Path, "/internal/brokerservers/")
	parts := strings.Split(rest, "/")

	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case len(parts) == 1 && parts[0] != "":
		serverID := parts[0]
		switch r.Method {
		case http.MethodPost:
			m.liveServers[serverID] = time.Now()
			m.record("server-starting", serverID, "")
		case http.MethodDelete:
			delete(m.liveServers, serverID)
			// A stopping server takes its connections with it.
			for k := range m.conns {
				if k.ServerID == serverID {
					delete(m.conns, k)
				}
			}
			m.record("server-stopping", serverID, "")
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

	case len(parts) == 3 && parts[1] == "connections":
		serverID, token := parts[0], parts[2]
		key := connKey{ServerID: serverID, Token: token}
		switch r.Method {
		case http.MethodPost:
			// client-connected doubles as the periodic freshness refresh;
			// only log the first sighting to keep the event log readable.
			if _, seen := m.conns[key]; !seen {
				m.record("client-connected", serverID, token)
			}
			m.conns[key] = time.Now()
			// A server we get connections from is implicitly live even if
			// we missed its server-starting (mock restarted mid-run).
			if _, ok := m.liveServers[serverID]; !ok {
				m.liveServers[serverID] = time.Now()
			}
		case http.MethodDelete:
			delete(m.conns, key)
			m.record("client-disconnected", serverID, token)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

	default:
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "{}")
}

// handleServersQuery answers which live servers hold a fresh stream for the
// hashed token: GET /servers/{hashedToken}
func (m *mock) handleServersQuery(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/servers/")

	m.mu.Lock()
	var servers []string
	now := time.Now()
	for k, seen := range m.conns {
		if k.Token != token {
			continue
		}
		if now.Sub(seen) > m.staleAfter {
			continue
		}
		if _, live := m.liveServers[k.ServerID]; !live {
			continue
		}
		servers = append(servers, k.ServerID)
	}
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"servers": servers})
}

type stateConn struct {
	ServerID string        `json:"serverId"`
	Token    string        `json:"token"`
	Age      time.Duration `json:"ageMs"`
	Stale    bool          `json:"stale"`
}

// handleState dumps everything for post-run validation.
func (m *mock) handleState(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	now := time.Now()
	conns := make([]stateConn, 0, len(m.conns))
	for k, seen := range m.conns {
		age := now.Sub(seen)
		conns = append(conns, stateConn{
			ServerID: k.ServerID,
			Token:    k.Token,
			Age:      age / time.Millisecond,
			Stale:    age > m.staleAfter,
		})
	}
	servers := make([]string, 0, len(m.liveServers))
	for id := range m.liveServers {
		servers = append(servers, id)
	}
	events := m.events
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"liveServers": servers,
		"connections": conns,
		"events":      events,
	})
}

// handleProbe fetches /healthz from every live server (only resolvable
// inside the compose network) so the orchestrator on the host can sample
// server state through the mock.
func (m *mock) handleProbe(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	servers := make([]string, 0, len(m.liveServers))
	for id := range m.liveServers {
		servers = append(servers, id)
	}
	m.mu.Unlock()

	client := &http.Client{Timeout: 3 * time.Second}
	results := make(map[string]string, len(servers))
	for _, id := range servers {
		resp, err := client.Get(fmt.Sprintf("http://%s:8080/healthz", id))
		if err != nil {
			results[id] = fmt.Sprintf("error: %v", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		results[id] = strings.TrimSpace(string(body))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}
