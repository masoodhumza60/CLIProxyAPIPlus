package executor

import (
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// QoderSession represents a single Qoder CLI subprocess session.
type QoderSession struct {
	SessionID  string
	Cmd        *exec.Cmd
	Stdin      io.WriteCloser
	Stdout     io.ReadCloser
	CreatedAt  time.Time
	LastUsedAt time.Time
	mu         sync.Mutex
}

// QoderSessionStore manages Qoder CLI subprocess sessions with TTL-based cleanup.
type QoderSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*QoderSession
	stopCh   chan struct{}
}

// NewQoderSessionStore creates a new session store with background cleanup.
func NewQoderSessionStore() *QoderSessionStore {
	store := &QoderSessionStore{
		sessions: make(map[string]*QoderSession),
		stopCh:   make(chan struct{}),
	}
	go store.cleanupLoop()
	return store
}

// Stop terminates the background cleanup loop.
func (s *QoderSessionStore) Stop() {
	close(s.stopCh)
}

// cleanupLoop periodically removes expired sessions.
func (s *QoderSessionStore) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Cleanup(5 * time.Minute)
		case <-s.stopCh:
			return
		}
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

// Delete removes a session and kills its CLI process.
func (s *QoderSessionStore) Delete(sessionID string) {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	if ok {
		s.closeSession(sess)
	}
}

// Cleanup removes sessions older than maxAge and kills their processes.
func (s *QoderSessionStore) Cleanup(maxAge time.Duration) {
	s.mu.Lock()
	now := time.Now()
	expired := make([]*QoderSession, 0)
	for id, sess := range s.sessions {
		sess.mu.Lock()
		elapsed := now.Sub(sess.LastUsedAt)
		sess.mu.Unlock()
		if elapsed > maxAge {
			expired = append(expired, sess)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, sess := range expired {
		s.closeSession(sess)
	}
}

// UpdateLastUsed updates the session's last-used timestamp.
func (s *QoderSessionStore) UpdateLastUsed(sessionID string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.mu.Lock()
		sess.LastUsedAt = time.Now()
		sess.mu.Unlock()
	}
}

// closeSession kills the CLI process and closes pipes.
func (s *QoderSessionStore) closeSession(sess *QoderSession) {
	if sess == nil {
		return
	}
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	if sess.Stdout != nil {
		_ = sess.Stdout.Close()
	}
	if sess.Cmd != nil && sess.Cmd.Process != nil {
		_ = sess.Cmd.Process.Kill()
	}
	if sess.Cmd != nil {
		_ = sess.Cmd.Wait()
	}
}

// sessionCount returns the number of active sessions (for testing).
func (s *QoderSessionStore) sessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// ensure fmt is used
var _ = fmt.Sprintf
