package grpctunnel

import (
	"sync"

	"google.golang.org/grpc"
)

// connPool holds the fixed set of connections used by "direct" mode. Streams
// are spread across them by index and share them freely.
//
// Unlike connGroup it does not refcount: a connection's job is to exist, so
// that a server rolling or a network blip costs a fraction of capacity rather
// than all of it. That is a static decision about blast radius, so tying
// connection lifetime to how many streams happen to be riding one — the way
// the watermark pool does — would shrink resilience exactly when traffic is
// quiet and give it back only after the damage.
//
// Connections are dialed on first use rather than up front, so a server that
// is briefly unreachable at startup costs one stream's retry instead of
// blocking the agent.
type connPool struct {
	mu     sync.Mutex
	conns  []*grpc.ClientConn
	closed bool
}

func newConnPool(n int) *connPool {
	if n < 1 {
		n = 1
	}
	return &connPool{conns: make([]*grpc.ClientConn, n)}
}

func (p *connPool) size() int {
	return len(p.conns)
}

// get returns the connection at idx%size, dialing it if this is its first
// use. The returned connection belongs to the pool; callers must not close it.
func (p *connPool) get(idx int, dial func() (*grpc.ClientConn, error)) (*grpc.ClientConn, error) {
	i := idx % len(p.conns)

	p.mu.Lock()
	if !p.closed {
		if c := p.conns[i]; c != nil {
			p.mu.Unlock()
			return c, nil
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
	defer p.mu.Unlock()
	if p.closed {
		conn.Close()
		return nil, errPoolClosed
	}
	// Another worker may have won the race; keep the first and drop ours so
	// the pool never exceeds its configured connection count.
	if existing := p.conns[i]; existing != nil {
		conn.Close()
		return existing, nil
	}
	p.conns[i] = conn
	return conn, nil
}

func (p *connPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for i, c := range p.conns {
		if c != nil {
			c.Close()
			p.conns[i] = nil
		}
	}
}
