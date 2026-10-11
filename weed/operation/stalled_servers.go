package operation

import (
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// StalledServers remembers, for a while, the volume servers an upload attempt
// stalled or failed on, so later assignments can be steered around them. It
// is safe for concurrent use.
type StalledServers struct {
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	until map[string]time.Time
}

// NewStalledServers returns a record whose entries expire ttl after the
// failure that added them.
func NewStalledServers(ttl time.Duration) *StalledServers {
	return &StalledServers{ttl: ttl, now: time.Now, until: make(map[string]time.Time)}
}

// Mark records server as stalled for the record's ttl from now.
func (s *StalledServers) Mark(server string) {
	if server == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.until[server] = s.now().Add(s.ttl)
}

// firstStalled returns the first of servers that is still marked stalled.
func (s *StalledServers) firstStalled(servers []string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, server := range servers {
		until, ok := s.until[server]
		if !ok {
			continue
		}
		if now.Before(until) {
			return server, true
		}
		delete(s.until, server)
	}
	return "", false
}

// replicaFailurePattern matches the servers a primary names in its
// "failed to write to replicas for volume N: [server]: cause" error, one
// "[server]: " per failed replica.
var replicaFailurePattern = regexp.MustCompile(`\[([^\]\s]+)\]: `)

// noteFailure records which of servers, the assignment's primary first, the
// failed attempt points at. An attempt with no response (it timed out, or the
// primary was unreachable) or a 429 from a full upload admission marks the
// primary: the client cannot tell whether the primary or a replica behind it
// held the request, and the primary is in every write to the volume either
// way. A replicated write that failed marks the replicas the primary names.
// Other failures are about the volume, not a server, and the reassignment
// that follows already moves off it.
func (s *StalledServers) noteFailure(servers []string, err error) {
	var se *uploadStatusError
	if !errors.As(err, &se) || len(servers) == 0 {
		return
	}
	switch {
	case se.StatusCode == 0 || se.StatusCode == http.StatusTooManyRequests:
		s.Mark(servers[0])
	case se.StatusCode >= 500:
		for _, m := range replicaFailurePattern.FindAllStringSubmatch(se.Error(), -1) {
			for _, server := range servers[1:] {
				if server == m[1] {
					s.Mark(server)
				}
			}
		}
	}
}
