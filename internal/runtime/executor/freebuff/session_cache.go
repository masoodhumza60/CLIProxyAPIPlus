package freebuff

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// admittedSession is what the protocol needs to resume a session later.
type admittedSession struct {
	instanceID string
	model      string
}

var (
	admittedMu sync.Mutex
	admitted   = make(map[string]admittedSession)
)

// sessionKey identifies an account without holding its token.
//
// The cache is process-wide and long-lived, so it is keyed by a digest rather
// than the token itself: a token that leaked out of this map would be a live
// credential sitting in memory for the life of the process, and a digest
// identifies an account just as well for the only question ever asked of it,
// which is "did this account already hold a session".
func sessionKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}

// recallSession returns the instance id an account is still holding, but only
// for the model it was admitted for. Admission is per model: the gate
// vocabulary includes model_locked and model_unavailable, so a session held for
// one model says nothing about another and must not be presented as if it did.
func recallSession(apiKey, model string) string {
	admittedMu.Lock()
	defer admittedMu.Unlock()
	held, ok := admitted[sessionKey(apiKey)]
	if !ok {
		return ""
	}
	if model != "" && held.model != "" && held.model != model {
		return ""
	}
	return held.instanceID
}

// rememberSession records the session an account now holds.
func rememberSession(apiKey, instanceID, model string) {
	if instanceID == "" {
		return
	}
	admittedMu.Lock()
	defer admittedMu.Unlock()
	admitted[sessionKey(apiKey)] = admittedSession{instanceID: instanceID, model: model}
}

// forgetSession drops an account's held session.
//
// Called whenever the server refuses to resume it: the entry has already proven
// unusable, and keeping it would make every later request attempt the same dead
// instance first.
func forgetSession(apiKey string) {
	admittedMu.Lock()
	defer admittedMu.Unlock()
	delete(admitted, sessionKey(apiKey))
}
