package acceptfile

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// PoolManager handles _POOL variable resolution with round-robin rotation.
// When an environment variable like GITHUB_API_POOL is set to a comma-separated
// list of values (e.g., "https://api1.github.com,https://api2.github.com"),
// the pool manager rotates through them on each resolution.
type PoolManager struct {
	mu    sync.RWMutex
	pools map[string]*poolEntry
}

type poolEntry struct {
	values  []string
	counter atomic.Uint64
}

func NewPoolManager() *PoolManager {
	return &PoolManager{
		pools: make(map[string]*poolEntry),
	}
}

// getPool returns the pool entry for the given variable name, creating it if needed.
func (pm *PoolManager) getPool(varName string) *poolEntry {
	pm.mu.RLock()
	entry, exists := pm.pools[varName]
	pm.mu.RUnlock()
	if exists {
		return entry
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Double-check after acquiring write lock.
	if entry, exists = pm.pools[varName]; exists {
		return entry
	}

	poolValue := os.Getenv(varName + "_POOL")
	if poolValue == "" {
		return nil
	}

	values := strings.Split(poolValue, ",")
	trimmed := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			trimmed = append(trimmed, v)
		}
	}
	if len(trimmed) == 0 {
		return nil
	}

	entry = &poolEntry{values: trimmed}
	pm.pools[varName] = entry
	return entry
}

// Next returns the next value from the pool using round-robin.
func (pe *poolEntry) Next() string {
	idx := pe.counter.Add(1) - 1
	return pe.values[idx%uint64(len(pe.values))]
}

// Peek returns the value Next would return, without consuming the slot.
// Startup validation uses it so checking a pool does not shift which member the
// first request lands on.
func (pe *poolEntry) Peek() string {
	idx := pe.counter.Load()
	return pe.values[idx%uint64(len(pe.values))]
}

// reEnvVar matches ${VAR_NAME} patterns in strings.
var reEnvVar = regexp.MustCompile(`\$\{([^}]+)\}`)

// ResolvePoolVars resolves any ${VAR} references in the string, checking for
// _POOL variants first (round-robin), then falling back to regular env vars.
func (pm *PoolManager) ResolvePoolVars(s string) string {
	return pm.resolveVars(s, true)
}

// resolveVars resolves ${VAR} references. advance is false for startup
// validation, which must observe a pool without consuming a slot.
//
// The pool is checked before the plain environment variable, matching
// snyk-broker's replace(). Resolution has to happen on the reference rather
// than on an already-expanded string: a variable that exists only as VAR_POOL
// expands to the empty string, leaving nothing to rotate.
func (pm *PoolManager) resolveVars(s string, advance bool) string {
	return reEnvVar.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[2 : len(match)-1] // strip ${ and }

		if entry := pm.getPool(varName); entry != nil {
			if advance {
				return entry.Next()
			}
			return entry.Peek()
		}

		if val := os.Getenv(varName); val != "" {
			return val
		}

		return match
	})
}
