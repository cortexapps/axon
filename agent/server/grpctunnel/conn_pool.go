package grpctunnel

import (
	"sync"

	"google.golang.org/grpc"
)

// connPool holds the agent's fixed set of connections. Streams are spread
// across them by worker id and share them freely.
//
// Connections are not refcounted: a connection's job is to exist, so that a
// server rolling or a network blip costs a fraction of capacity rather than
// all of it. That is a static decision about blast radius, so tying
// connection lifetime to how many streams happen to be riding one would
// shrink resilience exactly when traffic is quiet — and incidents do not
// wait for peak traffic.
//
// Connections are dialed on first use rather than up front, so a server that
// is briefly unreachable at startup costs one stream's retry instead of
// blocking the agent.
//
// Each connection remembers the target it was dialed for. A grpc.ClientConn
// redials its own target forever, so when registration moves the agent to a
// different server, the connection for the old one is replaced on next use
// rather than kept until the process exits.
type connPool struct {
	mu     sync.Mutex
	conns  []pooledConn
	closed bool
}

type pooledConn struct {
	conn   *grpc.ClientConn
	target string
}

func newConnPool(n int) *connPool {
	if n < 1 {
		n = 1
	}
	return &connPool{conns: make([]pooledConn, n)}
}

func (p *connPool) size() int {
	return len(p.conns)
}

// get returns the connection at idx%size for target, dialing it if this is
// its first use or the pooled connection was dialed for a different target.
// A replaced connection is closed, which ends any streams still on it; they
// reopen on the new one. The returned connection belongs to the pool;
// callers must not close it.
func (p *connPool) get(idx int, target string, dial func() (*grpc.ClientConn, error)) (*grpc.ClientConn, error) {
	i := idx % len(p.conns)

	p.mu.Lock()
	if !p.closed {
		if pc := p.conns[i]; pc.conn != nil && pc.target == target {
			p.mu.Unlock()
			return pc.conn, nil
		}
	}
	p.mu.Unlock()

	// Dial outside the lock: it is cheap (grpc.NewClient does not block on a
	// TCP handshake) but not free, and holding the lock would serialize every
	// worker starting up at once.
	conn, err := dial()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		conn.Close()
		return nil, errPoolClosed
	}
	// Another worker may have won the race; keep the first and drop ours so
	// the pool never exceeds its configured connection count.
	existing := p.conns[i]
	if existing.conn != nil && existing.target == target {
		p.mu.Unlock()
		conn.Close()
		return existing.conn, nil
	}
	p.conns[i] = pooledConn{conn: conn, target: target}
	p.mu.Unlock()

	// Close the stale connection outside the lock: Close waits for the
	// connection's goroutines, and other workers need the pool meanwhile.
	if existing.conn != nil {
		existing.conn.Close()
	}
	return conn, nil
}

func (p *connPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for i, pc := range p.conns {
		if pc.conn != nil {
			pc.conn.Close()
			p.conns[i] = pooledConn{}
		}
	}
}
