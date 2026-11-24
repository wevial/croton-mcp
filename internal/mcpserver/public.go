package mcpserver

import (
	"sync"
	"time"
)

// Only a verified bearer supplies a client key. The empty key is shared by
// all requests without a verified client, across MCP and OAuth routes.
type publicAdmission struct {
	inFlight  chan struct{}
	mu        sync.Mutex
	recent    map[string][]time.Time
	nextSweep time.Time
}

func (p *publicAdmission) allow(client string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)
	if !now.Before(p.nextSweep) {
		for key, times := range p.recent {
			if !times[len(times)-1].After(cutoff) {
				delete(p.recent, key)
			}
		}
		p.nextSweep = now.Add(time.Minute)
	}

	times := p.recent[client]
	for len(times) > 0 && !times[0].After(cutoff) {
		times = times[1:]
	}
	limit := 120
	if client == "" {
		limit = 30
	}
	if len(times) >= limit {
		return false
	}

	p.recent[client] = append(times, now)
	return true
}
