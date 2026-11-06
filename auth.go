package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

const (
	loginLinkTTL   = 15 * time.Minute
	inviteLinkTTL  = 7 * 24 * time.Hour
	directLinkTTL  = 2 * time.Minute
	sessionTTL     = 30 * 24 * time.Hour
	maxLoginPerKey = 3
	// A whole classroom can sit behind one campus address, so the per-address cap is generous.
	maxLoginPerIP = 300
)

type magicRec struct {
	App     string    `json:"app"`
	Email   string    `json:"email"`
	Expires time.Time `json:"expires"`
	// Direct links skip the confirm page. Only the dashboard issues them, for the
	// owner's own browser, so a mail scanner never sees one.
	Direct bool `json:"direct"`
}

type sessionRec struct {
	App     string    `json:"app"`
	Email   string    `json:"email"`
	Expires time.Time `json:"expires"`
}

// AuthStore holds one-time sign-in links and sessions. Only SHA-256 hashes of the
// tokens are kept, so a leaked auth.json can't be replayed.
type AuthStore struct {
	mu       sync.Mutex
	path     string
	now      func() time.Time
	Magic    map[string]magicRec   `json:"magic"`
	Sessions map[string]sessionRec `json:"sessions"`
}

func NewAuthStore(path string) (*AuthStore, error) {
	a := &AuthStore{path: path, now: time.Now, Magic: map[string]magicRec{}, Sessions: map[string]sessionRec{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, a); err != nil {
		return nil, err
	}
	if a.Magic == nil {
		a.Magic = map[string]magicRec{}
	}
	if a.Sessions == nil {
		a.Sessions = map[string]sessionRec{}
	}
	return a, nil
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (a *AuthStore) save() error {
	now := a.now()
	for k, m := range a.Magic {
		if now.After(m.Expires) {
			delete(a.Magic, k)
		}
	}
	for k, s := range a.Sessions {
		if now.After(s.Expires) {
			delete(a.Sessions, k)
		}
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func (a *AuthStore) NewMagic(app, email string, ttl time.Duration, direct bool) (string, error) {
	tok := newToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Magic[hashToken(tok)] = magicRec{App: app, Email: email, Expires: a.now().Add(ttl), Direct: direct}
	return tok, a.save()
}

// PeekMagic reads a link without using it up, so the confirm page can show who
// is about to sign in.
func (a *AuthStore) PeekMagic(tok string) (magicRec, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.Magic[hashToken(tok)]
	if !ok || a.now().After(m.Expires) {
		return magicRec{}, false
	}
	return m, true
}

// ConsumeMagic uses a link up. It returns false if the link is unknown, expired or already used.
func (a *AuthStore) ConsumeMagic(tok string) (magicRec, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := hashToken(tok)
	m, ok := a.Magic[h]
	if !ok {
		return magicRec{}, false
	}
	delete(a.Magic, h)
	if err := a.save(); err != nil || a.now().After(m.Expires) {
		return magicRec{}, false
	}
	return m, true
}

func (a *AuthStore) NewSession(app, email string) (string, error) {
	tok := newToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Sessions[hashToken(tok)] = sessionRec{App: app, Email: email, Expires: a.now().Add(sessionTTL)}
	return tok, a.save()
}

func (a *AuthStore) Session(tok string) (sessionRec, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.Sessions[hashToken(tok)]
	if !ok || a.now().After(s.Expires) {
		return sessionRec{}, false
	}
	return s, true
}

// Drop removes every session and pending link for email on app. An empty email drops the whole app.
func (a *AuthStore) Drop(app, email string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, s := range a.Sessions {
		if s.App == app && (email == "" || s.Email == email) {
			delete(a.Sessions, k)
		}
	}
	for k, m := range a.Magic {
		if m.App == app && (email == "" || m.Email == email) {
			delete(a.Magic, k)
		}
	}
	return a.save()
}

// limiter caps how often a key may act within a window, to stop sign-in email floods.
type limiter struct {
	mu   sync.Mutex
	now  func() time.Time
	hits map[string][]time.Time
}

func newLimiter() *limiter {
	return &limiter{now: time.Now, hits: map[string][]time.Time{}}
}

func (l *limiter) Allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= max {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}
