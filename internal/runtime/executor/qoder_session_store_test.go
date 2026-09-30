package executor

import (
	"testing"
	"time"
)

func TestSessionStorePutGetDelete(t *testing.T) {
	store := NewQoderSessionStore()
	session := &QoderSession{SessionID: "test-1"}

	store.Put("test-1", session)
	got, ok := store.Get("test-1")
	if !ok || got != session {
		t.Fatal("expected to get session")
	}

	store.Delete("test-1")
	_, ok = store.Get("test-1")
	if ok {
		t.Fatal("expected session to be deleted")
	}
}

func TestSessionStoreCleanup(t *testing.T) {
	store := NewQoderSessionStore()
	session := &QoderSession{
		SessionID:  "old-session",
		CreatedAt:  time.Now().Add(-1 * time.Hour),
		LastUsedAt: time.Now().Add(-1 * time.Hour),
	}
	store.Put("old-session", session)

	store.Cleanup(30 * time.Minute)
	_, ok := store.Get("old-session")
	if ok {
		t.Fatal("expected old session to be cleaned up")
	}
}
