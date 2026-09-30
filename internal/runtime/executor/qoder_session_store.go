package executor

import (
	"sync"
	"time"
)

// QoderSession represents a single Qoder CLI subprocess session.
type QoderSession struct {
	SessionID  string
	Cmd        interface{} // *exec.Cmd, set by executor
	Stdin      interface{} // io.WriteCloser
	Stdout     interface{} // io.ReadCloser
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// QoderSessionStore manages Qoder CLI subprocess sessions with TTL-based cleanup.
type QoderSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*QoderSession
}

// NewQoderSessionStore creates a new session store.
func NewQoderSessionStore() *QoderSessionStore {
	return &QoderSessionStore{
		sessions: make(map[string]*QoderSession),
	}
}

// Get retrieves a session by ID.
func (s *QoderSessionStore) Get(sessionID string) (*QoderSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	return sess, ok
}

// Put stores a session.
func (s *QoderSessionStore) Put(sessionID string, session *QoderSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = session
}

// Delete removes a session.
func (s *QoderSessionStore) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// Cleanup removes sessions older than maxAge.
func (s *QoderSessionStore) Cleanup(maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, sess := range s.sessions {
		if now.Sub(sess.LastUsedAt) > maxAge {
			delete(s.sessions, id)
		}
	}
}
