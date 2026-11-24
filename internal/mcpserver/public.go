package mcpserver

import (
	"sync"
	"time"
)

// MCP and OAuth share the empty key for requests without a verified bearer client.
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
