package operation

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// Write timeouts, all progress-based (see ProgressWatch): an attempt is
// abandoned only when its body moves no byte for the stall timeout, or no
// response arrives within the response timeout after the body was sent. A
// FUSE close() must be answered within the 60 s request deadline, with room
// for a second attempt elsewhere, so the mount waits less for a response than
// the S3 gateway, whose clients wait minutes. A healthy 2 MiB chunk upload
// measured 0.06 to 0.24 s under load; a server that accepts no byte for 10 s
// has stopped.
const (
	UploadStallTimeout           = 10 * time.Second
	MountUploadResponseTimeout   = 20 * time.Second
	GatewayUploadResponseTimeout = 30 * time.Second
)

// StalledServerTTL is how long writers avoid a volume server after an attempt
// stalled on it.
const StalledServerTTL = time.Minute

// StalledServers remembers, for a while, the volume servers an upload attempt
// stalled or failed on, so later assignments can be steered around them. It
// is safe for concurrent use, and a nil *StalledServers avoids nothing.
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
	if s == nil || server == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.until[server] = s.now().Add(s.ttl)
}

// FirstStalled returns the first of servers that is still marked stalled.
func (s *StalledServers) FirstStalled(servers []string) (string, bool) {
	if s == nil {
		return "", false
	}
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

// maxAssignmentsAvoidingStalled bounds how many assignments one write asks
// for while looking for a volume none of whose servers recently stalled.
const maxAssignmentsAvoidingStalled = 8

// Pick calls assign, which makes one assignment and returns the servers that
// hold the assigned volume, until one avoids every stalled server or
// maxAssignmentsAvoidingStalled are spent. The caller keeps the assignment
// from the last call: when no clean volume turns up the write still goes
// ahead, since a stalled server may have recovered.
func (s *StalledServers) Pick(assign func() ([]string, error)) error {
	for i := 0; ; i++ {
		servers, err := assign()
		if err != nil {
			return err
		}
		server, avoided := s.FirstStalled(servers)
		if !avoided || i+1 == maxAssignmentsAvoidingStalled {
			return nil
		}
		glog.V(1).Infof("skip an assignment on %v: %s stalled recently", servers, server)
	}
}

// replicaFailurePattern matches the servers a primary names in its
// "failed to write to replicas for volume N: [server]: cause" error, one
// "[server]: " per failed replica.
var replicaFailurePattern = regexp.MustCompile(`\[([^\]\s]+)\]: `)

// NoteFailure records which of servers, the assignment's primary first, a
// failed write attempt points at. An attempt with no response (it timed out,
// or the primary was unreachable) or a 429 from a full upload admission marks
// the primary: the client cannot tell whether the primary or a replica behind
// it held the request, and the primary is in every write to the volume either
// way. A replicated write that failed marks the replicas the primary names.
// Other failures are about the volume, not a server, and the reassignment
// that follows already moves off it.
func (s *StalledServers) NoteFailure(servers []string, err error) {
	if s == nil || len(servers) == 0 {
		return
	}
	var se *uploadStatusError
	if !errors.As(err, &se) {
		return
	}
	switch {
	case se.StatusCode == 0 || se.StatusCode == http.StatusTooManyRequests:
		// A write this process cancelled itself, such as the other holders
		// of a fan-out after one failed, says nothing about the server.
		if !strings.Contains(se.Error(), context.Canceled.Error()) {
			s.Mark(servers[0])
		}
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

// NewUploadStatusError wraps err from a write to a volume server that answered
// with statusCode, or 0 when it never answered, so ShouldReassignUpload and
// NoteFailure can judge writes made outside this package.
func NewUploadStatusError(statusCode int, err error) error {
	return &uploadStatusError{StatusCode: statusCode, err: err}
}
