package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxSiteBytes = 50 << 20

var (
	ErrNotFound      = errors.New("app not found")
	ErrInvalidName   = errors.New("app name must match [a-z0-9-]{1,32}")
	ErrInvalidEmail  = errors.New("enter a full email address like name@example.com")
	ErrInvalidAccess = errors.New("access must be invited, org or public")
	ErrInvalidEnvKey = errors.New("setting names look like API_KEY: capitals, digits and underscores")
	ErrNoPrevious    = errors.New("no earlier version to roll back to")
	nameRe           = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// OwnerEmail is the identity of the person running the server. It is never listed
// among an app's People, but sessions opened from the dashboard carry it.
const OwnerEmail = "owner"

type Person struct {
	Email    string    `json:"email"`
	Added    time.Time `json:"added"`
	LastSeen time.Time `json:"lastSeen"`
}

// Access says who may open an app besides the owner and the people invited to it.
type Access string

const (
	AccessInvited Access = "invited"
	AccessOrg     Access = "org"
	AccessPublic  Access = "public"
)

func ParseAccess(s string) (Access, error) {
	switch a := Access(s); a {
	case AccessInvited, AccessOrg, AccessPublic:
		return a, nil
	}
	return "", ErrInvalidAccess
}

type App struct {
	Name     string            `json:"name"`
	Access   Access            `json:"access"`
	People   []Person          `json:"people"`
	Env      map[string]string `json:"env,omitempty"`
	Created  time.Time         `json:"created"`
	Deployed time.Time         `json:"deployed"`

	// LegacyPublic is read from data written before Access existed.
	LegacyPublic bool `json:"public,omitempty"`
}

func (a *App) HasPerson(email string) bool {
	for _, p := range a.People {
		if p.Email == email {
			return true
		}
	}
	return false
}

// NormalizeEmail returns the canonical form of an address, so "Bob@X.com " and
// "bob@x.com" are the same person.
func NormalizeEmail(s string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil || addr.Name != "" {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(addr.Address), nil
}

type Store struct {
	mu  sync.Mutex
	dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func ValidName(n string) error {
	if !nameRe.MatchString(n) {
		return ErrInvalidName
	}
	return nil
}

func (s *Store) appDir(name string) string  { return filepath.Join(s.dir, "apps", name) }
func (s *Store) SiteDir(name string) string { return filepath.Join(s.appDir(name), "site") }
func (s *Store) prevDir(name string) string { return filepath.Join(s.appDir(name), "site.prev") }

func (s *Store) CanRollback(name string) bool {
	_, err := os.Stat(s.prevDir(name))
	return err == nil
}

func newToken() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Store) Get(name string) (*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(name)
}

func (s *Store) load(name string) (*App, error) {
	if err := ValidName(name); err != nil {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.appDir(name), "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var a App
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if a.Access == "" {
		a.Access = AccessInvited
		if a.LegacyPublic {
			a.Access = AccessPublic
		}
	}
	a.LegacyPublic = false
	return &a, nil
}

func (s *Store) save(a *App) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.appDir(a.Name), "meta.json"), b)
}

func (s *Store) List() ([]*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(filepath.Join(s.dir, "apps"))
	if err != nil {
		return nil, err
	}
	var out []*App
	for _, e := range ents {
		a, err := s.load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Update applies fn to the app under the store lock and persists the result.
func (s *Store) Update(name string, fn func(*App)) (*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.load(name)
	if err != nil {
		return nil, err
	}
	fn(a)
	return a, s.save(a)
}

func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.load(name); err != nil {
		return err
	}
	return os.RemoveAll(s.appDir(name))
}

// Invite adds email to the app's people if it isn't there yet.
func (s *Store) Invite(name, email string) (Person, error) {
	var out Person
	_, err := s.Update(name, func(a *App) {
		for _, p := range a.People {
			if p.Email == email {
				out = p
				return
			}
		}
		out = Person{Email: email, Added: time.Now().UTC()}
		a.People = append(a.People, out)
	})
	return out, err
}

func (s *Store) Revoke(name, email string) error {
	_, err := s.Update(name, func(a *App) {
		kept := a.People[:0]
		for _, p := range a.People {
			if p.Email != email {
				kept = append(kept, p)
			}
		}
		a.People = kept
	})
	return err
}

func (s *Store) Touch(name, email string) error {
	_, err := s.Update(name, func(a *App) {
		for i := range a.People {
			if a.People[i].Email == email {
				a.People[i].LastSeen = time.Now().UTC()
			}
		}
	})
	return err
}

// Rollback swaps the live site with the one before the last deploy, so rolling
// back twice returns to where you started.
func (s *Store) Rollback(name string) (*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.load(name)
	if err != nil {
		return nil, err
	}
	site, prev, tmp := s.SiteDir(name), s.prevDir(name), filepath.Join(s.appDir(name), "site.swap")
	if _, err := os.Stat(prev); err != nil {
		return nil, ErrNoPrevious
	}
	for _, step := range [][2]string{{site, tmp}, {prev, site}, {tmp, prev}} {
		if err := os.Rename(step[0], step[1]); err != nil {
			return nil, err
		}
	}
	a.Deployed = time.Now().UTC()
	return a, s.save(a)
}

var envKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

const maxEnvValue = 4 << 10

// SetEnv stores a setting handlers read as env.KEY. An empty value removes it.
func (s *Store) SetEnv(name, key, value string) error {
	if !envKeyRe.MatchString(key) {
		return ErrInvalidEnvKey
	}
	if len(value) > maxEnvValue {
		return fmt.Errorf("setting values are limited to %d bytes", maxEnvValue)
	}
	_, err := s.Update(name, func(a *App) {
		if value == "" {
			delete(a.Env, key)
			return
		}
		if a.Env == nil {
			a.Env = map[string]string{}
		}
		a.Env[key] = value
	})
	return err
}

// Deploy replaces the app's site with the contents of the zip, creating the app if new.
// The swap is rename-based so viewers never see a half-extracted site.
func (s *Store) Deploy(name string, zipData []byte) (*App, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("bad zip: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.appDir(name), 0o755); err != nil {
		return nil, err
	}
	staging := filepath.Join(s.appDir(name), "site.new")
	os.RemoveAll(staging)
	if err := extract(zr, staging); err != nil {
		os.RemoveAll(staging)
		return nil, err
	}
	site, prev := s.SiteDir(name), s.prevDir(name)
	os.RemoveAll(prev)
	if err := os.Rename(site, prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(staging, site); err != nil {
		return nil, err
	}

	a, err := s.load(name)
	if errors.Is(err, ErrNotFound) {
		a = &App{Name: name, Created: time.Now().UTC()}
	} else if err != nil {
		return nil, err
	}
	a.Deployed = time.Now().UTC()
	return a, s.save(a)
}

func extract(zr *zip.Reader, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var total int64
	for _, f := range zr.File {
		if !filepath.IsLocal(f.Name) {
			return fmt.Errorf("unsafe path in zip: %q", f.Name)
		}
		target := filepath.Join(dst, f.Name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxSiteBytes-total+1))
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
		total += n
		if total > maxSiteBytes {
			return fmt.Errorf("site exceeds %d MB", maxSiteBytes>>20)
		}
	}
	return nil
}

func (s *Store) kvPath(name string) string { return filepath.Join(s.appDir(name), "kv.json") }

func (s *Store) loadKV(name string) (map[string]json.RawMessage, error) {
	kv := map[string]json.RawMessage{}
	b, err := os.ReadFile(s.kvPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return kv, nil
	}
	if err != nil {
		return nil, err
	}
	return kv, json.Unmarshal(b, &kv)
}

func (s *Store) KVAll(name string) (map[string]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadKV(name)
}

// KVSet stores val under key; a nil val deletes the key.
func (s *Store) KVSet(name, key string, val json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kv, err := s.loadKV(name)
	if err != nil {
		return err
	}
	if val == nil {
		delete(kv, key)
	} else {
		kv[key] = val
	}
	b, err := json.Marshal(kv)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.kvPath(name), b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
