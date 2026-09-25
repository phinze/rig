package mux

import (
	"sync"
	"time"
)

// Breaker remembers which remote endpoints recently failed to connect, so a
// host that has dropped off the network costs a board one timeout rather than
// one per call for as long as it stays gone. It is keyed by whatever the
// backend dials (a Rex endpoint, an ssh host). Only a failure to connect
// trips it; a server that answered, even with an error, is up.
type Breaker struct {
	For time.Duration

	mu    sync.Mutex
	until map[string]time.Time
}

// Down reports whether key failed to connect within the last For.
func (b *Breaker) Down(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.until[key])
}

// Note records one call's outcome: unreachable trips the breaker for key,
// anything else resets it.
func (b *Breaker) Note(key string, unreachable bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until == nil {
		b.until = map[string]time.Time{}
	}
	if unreachable {
		b.until[key] = time.Now().Add(b.For)
	} else {
		delete(b.until, key)
	}
}
