package tacos

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
)

// ProviderSessions holds the in-memory provider session tokens for one
// `mgcl run -- <command>` child. It stores only SHA-256 digests; tokens are
// never persisted, logged or included in errors.
type ProviderSessions struct {
	mu      sync.RWMutex
	digests [][sha256.Size]byte
	closed  bool
}

// NewProviderSessions returns an empty, active session set.
func NewProviderSessions() *ProviderSessions {
	return &ProviderSessions{}
}

// Issue creates one random token of at least 32 bytes and records only its
// SHA-256 digest.
func (p *ProviderSessions) Issue() (string, error) {
	if p == nil {
		return "", errors.New("provider sessions are not configured")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("generate provider token")
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return "", errors.New("provider sessions are closed")
	}
	p.digests = append(p.digests, digest)
	return token, nil
}

// Authorize reports whether token belongs to a live session. The comparison is
// constant-time per stored digest.
func (p *ProviderSessions) Authorize(token string) bool {
	if p == nil || token == "" {
		return false
	}
	digest := sha256.Sum256([]byte(token))

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return false
	}
	for i := range p.digests {
		if subtle.ConstantTimeCompare(p.digests[i][:], digest[:]) == 1 {
			return true
		}
	}
	return false
}

// RevokeAll invalidates every session and closes the session set.
func (p *ProviderSessions) RevokeAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.digests {
		for j := range p.digests[i] {
			p.digests[i][j] = 0
		}
	}
	p.digests = nil
	p.closed = true
}
