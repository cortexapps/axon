// leakprobe calls the agent's relay registration repeatedly through a
// transport with a small per-host connection limit, and counts open
// connections. If registration leaks connections, a call eventually waits
// for a free connection, and so does any other request on the same client.
//
//	CORTEX_API_TOKEN=... go run ./test/leakprobe -alias relay-staging
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker"
)

var openConns atomic.Int32

type countedConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *countedConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		openConns.Add(-1)
	}
	return c.Conn.Close()
}

func main() {
	base := flag.String("base", "https://api.staging.getcortexapp.com", "Cortex API base URL")
	integration := flag.String("integration", "github", "relay integration")
	alias := flag.String("alias", "relay-staging", "relay alias that has a configuration")
	limit := flag.Int("limit", 8, "MaxConnsPerHost for the probe transport")
	count := flag.Int("n", 40, "number of registrations")
	interval := flag.Duration("interval", 500*time.Millisecond, "time between registrations")
	wait := flag.Duration("wait", 15*time.Second, "how long a call may take before it counts as blocked")
	flag.Parse()

	token := os.Getenv("CORTEX_API_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "CORTEX_API_TOKEN is not set")
		os.Exit(2)
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        *limit * 2,
		MaxIdleConnsPerHost: *limit,
		MaxConnsPerHost:     *limit,
		IdleConnTimeout:     90 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			openConns.Add(1)
			return &countedConn{Conn: c}, nil
		},
	}
	client := &http.Client{Transport: transport}

	reg := snykbroker.NewRegistration(config.AgentConfig{
		CortexApiBaseUrl: *base,
		CortexApiToken:   token,
		InstanceId:       "leak-probe",
	}, client)

	for i := 1; i <= *count; i++ {
		done := make(chan error, 1)
		go func() {
			_, err := reg.Register(common.Integration(*integration), *alias)
			done <- err
		}()

		select {
		case err := <-done:
			fmt.Printf("register %2d ok=%v open_conns=%d\n", i, err == nil, openConns.Load())
			if err != nil {
				fmt.Println("  err:", err)
			}
		case <-time.After(*wait):
			fmt.Printf("register %2d BLOCKED after %s, open_conns=%d\n", i, *wait, openConns.Load())
			probeSameClient(client, *base, token, *wait)
			os.Exit(1)
		}
		time.Sleep(*interval)
	}
	fmt.Printf("no block after %d registrations, open_conns=%d\n", *count, openConns.Load())
}

// probeSameClient shows that a normal API request on the same client also
// waits, as the agent's /cortex-api proxy does when the pool is full.
func probeSameClient(client *http.Client, base, token string, wait time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/v1/catalog?pageSize=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("catalog GET on same client: FAILED after %s: %v\n", time.Since(start).Round(time.Millisecond), err)
		return
	}
	resp.Body.Close()
	fmt.Printf("catalog GET on same client: status %d in %s\n", resp.StatusCode, time.Since(start).Round(time.Millisecond))
}
