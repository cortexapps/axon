package grpctunnel

import (
	"sync"

	"google.golang.org/grpc"
)

// connGroup hands out grpc.ClientConns shared by several stream workers, for
// the "mux" connection mode. Workers address a connection by index; the first
// worker to ask for an index dials it, later workers reuse it, and the
// connection closes once the last worker releases it.
//
// The pool and conns modes don't use this — there, every stream owns its own
// connection and closes it directly.
type connGroup struct {
	mu      sync.Mutex
	entries map[int]*connEntry
}

// connEntry is one shared connection plus the bookkeeping needed to close it
// exactly once and to hold a single server-id slot on behalf of all the
// streams riding on it.
type connEntry struct {
	conn *grpc.ClientConn
	refs int

	// serverID is learned from the first ServerHello on this connection and
	// is the same for every stream on it (a ClientConn under the default
	// pick_first balancer talks to exactly one backend). slotHeld records
	// whether we successfully took a server-id slot for it, so release
	// gives back exactly what it took.
	serverID string
	slotHeld bool
}

func newConnGroup() *connGroup {
	return &connGroup{entries: make(map[int]*connEntry)}
}

// acquire returns the connection at idx, dialing it via dial if this is the
// first reference. The returned entry has its refcount already incremented;
// the caller must pair every successful acquire with a release.
func (g *connGroup) acquire(idx int, dial func() (*grpc.ClientConn, error)) (*connEntry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if e, ok := g.entries[idx]; ok {
		e.refs++
		return e, nil
	}

	conn, err := dial()
	if err != nil {
		return nil, err
	}
	e := &connEntry{conn: conn, refs: 1}
	g.entries[idx] = e
	return e, nil
}

// release drops one reference to the connection at idx. When the last
// reference goes it closes the connection and reports the server-id slot it
// was holding, so the caller can release that too (done outside the lock to
// keep connGroup free of tunnelClient's locks).
func (g *connGroup) release(idx int) (releasedServerID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.entries[idx]
	if !ok {
		return ""
	}
	e.refs--
	if e.refs > 0 {
		return ""
	}

	delete(g.entries, idx)
	if e.conn != nil {
		e.conn.Close()
	}
	if e.slotHeld {
		return e.serverID
	}
	return ""
}

// noteServerID records the server this connection landed on. It returns
// needSlot=true for the first stream on the connection, which is the one
// responsible for taking the server-id slot; that stream then reports the
// outcome via setSlotHeld.
func (g *connGroup) noteServerID(idx int, serverID string) (needSlot bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.entries[idx]
	if !ok {
		return false
	}
	if e.serverID == "" {
		e.serverID = serverID
		return true
	}
	// A reconnect under us can land the connection on a different backend;
	// keep the newest id so release gives back the right slot.
	e.serverID = serverID
	return false
}

func (g *connGroup) setSlotHeld(idx int, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.entries[idx]; ok {
		e.slotHeld = held
	}
}

// closeAll drops every connection, returning the server-id slots that were
// held so the caller can release them.
func (g *connGroup) closeAll() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	var held []string
	for idx, e := range g.entries {
		if e.conn != nil {
			e.conn.Close()
		}
		if e.slotHeld {
			held = append(held, e.serverID)
		}
		delete(g.entries, idx)
	}
	return held
}
