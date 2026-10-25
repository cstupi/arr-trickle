package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/scrypt"

	"github.com/cstupi/arr-trickle/internal/state"
)

// Password rules and scrypt cost (~32 MiB and tens of milliseconds per hash). The cost is stored with
// each hash, so it can be raised later.
const (
	MinPassword = 12
	MaxPassword = 1024
	ScryptN     = 1 << 15
	ScryptR     = 8
	ScryptP     = 1
)

// scryptN is ScryptN, lowered only by tests (hashing at full cost under the race detector is slow).
var scryptN = ScryptN

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I

// AuthError is a setup or password problem; the message is safe to show.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// PasswordHash is the stored form of a password (same JSON as arr-trickle 0.3's Python version).
type PasswordHash struct {
	Alg  string `json:"alg"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return b
}

// HashPassword hashes with scrypt and a random 16-byte salt.
func HashPassword(password string) PasswordHash {
	salt := randomBytes(16)
	dk, err := scrypt.Key([]byte(password), salt, scryptN, ScryptR, ScryptP, 32)
	if err != nil {
		panic(err) // only for invalid constant parameters
	}
	enc := base64.StdEncoding.EncodeToString
	return PasswordHash{"scrypt", scryptN, ScryptR, ScryptP, enc(salt), enc(dk)}
}

// VerifyPassword compares in constant time.
func VerifyPassword(password string, h PasswordHash) bool {
	salt, err1 := base64.StdEncoding.DecodeString(h.Salt)
	expected, err2 := base64.StdEncoding.DecodeString(h.Hash)
	if err1 != nil || err2 != nil || len(expected) == 0 || h.N > 1<<20 || h.R > 32 || h.P > 16 {
		return false
	}
	dk, err := scrypt.Key([]byte(password), salt, h.N, h.R, h.P, len(expected))
	return err == nil && hmac.Equal(dk, expected)
}

// CheckNewPassword enforces the password rules.
func CheckNewPassword(password, confirm string) error {
	switch {
	case len([]rune(password)) < MinPassword:
		return &AuthError{fmt.Sprintf("the password must be at least %d characters", MinPassword)}
	case len([]rune(password)) > MaxPassword:
		return &AuthError{fmt.Sprintf("the password must be at most %d characters", MaxPassword)}
	case !hmac.Equal([]byte(password), []byte(confirm)):
		return &AuthError{"the passwords do not match"}
	}
	return nil
}

func normalizeCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// codeDigest: the code has ~100 bits of entropy, so a plain hash is enough (no need for a slow KDF).
func codeDigest(code string) string {
	sum := sha256.Sum256([]byte(normalizeCode(code)))
	return hex.EncodeToString(sum[:])
}

// A hash compared against when there is no admin, so a login takes the same time either way.
var dummy = sync.OnceValue(func() PasswordHash { return HashPassword(hex.EncodeToString(randomBytes(16))) })

type admin struct {
	Version      int          `json:"version"`
	Username     string       `json:"username"`
	Password     PasswordHash `json:"password"`
	CredentialID string       `json:"credential_id"`
}

// AuthStore is auth.json (the admin) and setup-code (hash of the one-time code). Both are re-read on
// every use, so the `reset-password` command takes effect in a running server immediately.
type AuthStore struct {
	Path, CodePath string
}

// NewAuthStore uses files in stateDir.
func NewAuthStore(stateDir string) *AuthStore {
	return &AuthStore{filepath.Join(stateDir, "auth.json"), filepath.Join(stateDir, "setup-code")}
}

func (s *AuthStore) admin() *admin {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil
	}
	var a admin
	if json.Unmarshal(raw, &a) != nil || a.Username == "" || a.Password.Hash == "" {
		return nil
	}
	return &a
}

// HasAdmin reports whether the admin account exists.
func (s *AuthStore) HasAdmin() bool { return s.admin() != nil }

// CredentialID changes whenever the admin is (re)created; sessions from before a reset stop working.
func (s *AuthStore) CredentialID() (string, bool) {
	a := s.admin()
	if a == nil {
		return "", false
	}
	return a.CredentialID, true
}

// NewSetupCode makes a fresh one-time code, stores its hash, and returns it.
func (s *AuthStore) NewSetupCode() (string, error) {
	groups := make([]string, 4)
	for g := range groups {
		b := make([]byte, 5)
		for i := range b {
			b[i] = codeAlphabet[int(randomBytes(1)[0])%len(codeAlphabet)] // 256 % 32 == 0: no bias
		}
		groups[g] = string(b)
	}
	code := strings.Join(groups, "-")
	if err := os.MkdirAll(filepath.Dir(s.CodePath), 0o755); err != nil {
		return "", err
	}
	return code, state.AtomicWrite(s.CodePath, []byte(codeDigest(code)), 0o600)
}

// CheckSetupCode compares in constant time.
func (s *AuthStore) CheckSetupCode(code string) bool {
	raw, err := os.ReadFile(s.CodePath)
	expected := strings.TrimSpace(string(raw))
	return err == nil && expected != "" && hmac.Equal([]byte(codeDigest(code)), []byte(expected))
}

// CreateAdmin creates the single admin account with a valid setup code.
func (s *AuthStore) CreateAdmin(code, username, password, confirm string) error {
	if s.HasAdmin() {
		return &AuthError{"an admin already exists"}
	}
	if !s.CheckSetupCode(code) {
		return &AuthError{"the setup code is not valid"}
	}
	username = strings.TrimSpace(username)
	if n := len([]rune(username)); n < 1 || n > 64 {
		return &AuthError{"the username must be 1-64 characters"}
	}
	if err := CheckNewPassword(password, confirm); err != nil {
		return err
	}
	raw, _ := json.Marshal(admin{1, username, HashPassword(password), hex.EncodeToString(randomBytes(16))})
	if err := state.AtomicWrite(s.Path, raw, 0o600); err != nil {
		return err
	}
	if err := os.Remove(s.CodePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// VerifyLogin returns the credential id on success. It always does the full hash, so timing doesn't
// reveal whether the username exists.
func (s *AuthStore) VerifyLogin(username, password string) (string, bool) {
	a := s.admin()
	h, name := dummy(), ""
	if a != nil {
		h, name = a.Password, a.Username
	}
	if r := []rune(password); len(r) > MaxPassword {
		password = string(r[:MaxPassword])
	}
	ok := VerifyPassword(password, h)
	nameOK := hmac.Equal([]byte(strings.TrimSpace(username)), []byte(name))
	if a != nil && ok && nameOK {
		return a.CredentialID, true
	}
	return "", false
}

// Reset removes the admin (ending every session) and returns a fresh setup code.
func (s *AuthStore) Reset() (string, error) {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return s.NewSetupCode()
}

// Session is server-side; the cookie holds only its random id.
type Session struct {
	ID           string
	CSRF         string
	Created      time.Time
	LastSeen     time.Time
	CredentialID string // "" = not logged in yet (login/setup forms only)
	Flash        []string
}

// Authenticated reports whether the session is logged in.
func (s *Session) Authenticated() bool { return s.CredentialID != "" }

func token() string { return base64.RawURLEncoding.EncodeToString(randomBytes(32)) }

// Sessions holds every session in memory; a restart logs everyone out.
type Sessions struct {
	Absolute, Idle time.Duration
	Clock          func() time.Time
	AnonLifetime   time.Duration
	MaxAnon        int
	mu             sync.Mutex
	byID           map[string]*Session
}

// NewSessions makes a store with the given absolute and idle timeouts.
func NewSessions(absolute, idle time.Duration) *Sessions {
	return &Sessions{
		Absolute: absolute, Idle: min(idle, absolute), Clock: time.Now,
		AnonLifetime: time.Hour, MaxAnon: 1000, byID: map[string]*Session{},
	}
}

func (s *Sessions) expired(x *Session, now time.Time) bool {
	if x.Authenticated() {
		return now.Sub(x.Created) > s.Absolute || now.Sub(x.LastSeen) > s.Idle
	}
	return now.Sub(x.Created) > s.AnonLifetime
}

// Create starts a session; credentialID "" makes a not-logged-in one.
func (s *Sessions) Create(credentialID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	now := s.Clock()
	x := &Session{ID: token(), CSRF: token(), Created: now, LastSeen: now, CredentialID: credentialID}
	s.byID[x.ID] = x
	return x
}

// Get returns a live session and refreshes its idle timer.
func (s *Sessions) Get(id string) *Session {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.byID[id]
	if x == nil {
		return nil
	}
	now := s.Clock()
	if s.expired(x, now) {
		delete(s.byID, id)
		return nil
	}
	x.LastSeen = now
	return x
}

// Delete ends a session.
func (s *Sessions) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// AddFlash queues a message for the session's next page.
func (s *Sessions) AddFlash(x *Session, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x.Flash = append(x.Flash, msg)
}

// TakeFlash returns and clears the queued messages.
func (s *Sessions) TakeFlash(x *Session) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := x.Flash
	x.Flash = nil
	return out
}

func (s *Sessions) prune() {
	now := s.Clock()
	var anon []*Session
	for id, x := range s.byID {
		switch {
		case s.expired(x, now):
			delete(s.byID, id)
		case !x.Authenticated():
			anon = append(anon, x)
		}
	}
	if extra := len(anon) - s.MaxAnon + 1; extra > 0 { // bound memory: drop the oldest not-logged-in sessions
		sort.Slice(anon, func(i, j int) bool { return anon[i].Created.Before(anon[j].Created) })
		for _, x := range anon[:extra] {
			delete(s.byID, x.ID)
		}
	}
}

// RateLimiter throttles failed logins: exponential backoff per client IP after a few free attempts,
// plus a global ceiling so an attacker rotating addresses is slowed too.
type RateLimiter struct {
	Free         int
	MaxBackoff   time.Duration
	GlobalLimit  int
	GlobalWindow time.Duration
	Clock        func() time.Time
	mu           sync.Mutex
	ips          map[string]failures
	recent       []time.Time
}

type failures struct {
	n    int
	last time.Time
}

// NewRateLimiter has the production defaults: 5 free attempts, backoff up to 15 minutes, 30 failures per 10 minutes.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{Free: 5, MaxBackoff: 15 * time.Minute, GlobalLimit: 30, GlobalWindow: 10 * time.Minute, Clock: time.Now, ips: map[string]failures{}}
}

// RetryAfter is how long this client must wait (0 = allowed now).
func (l *RateLimiter) RetryAfter(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Clock()
	for len(l.recent) > 0 && now.Sub(l.recent[0]) > l.GlobalWindow {
		l.recent = l.recent[1:]
	}
	var wait time.Duration
	if len(l.recent) >= l.GlobalLimit {
		wait = l.recent[0].Add(l.GlobalWindow).Sub(now)
	}
	if f := l.ips[ip]; f.n >= l.Free {
		backoff := time.Duration(math.Min(float64(l.MaxBackoff), float64(time.Second)*math.Pow(2, float64(f.n-l.Free))))
		wait = max(wait, f.last.Add(backoff).Sub(now))
	}
	return max(wait, 0)
}

// Failure records a failed attempt.
func (l *RateLimiter) Failure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Clock()
	f := l.ips[ip]
	if f.n > 0 && now.Sub(f.last) > 2*l.MaxBackoff {
		f.n = 0 // quiet for a while: start over
	}
	l.ips[ip] = failures{f.n + 1, now}
	l.recent = append(l.recent, now)
	if len(l.recent) > 10*l.GlobalLimit+1000 { // bound memory
		l.recent = l.recent[len(l.recent)-l.GlobalLimit:]
	}
	if len(l.ips) > 10_000 { // bound memory; drop the stalest entries
		type kv struct {
			ip string
			t  time.Time
		}
		all := make([]kv, 0, len(l.ips))
		for k, v := range l.ips {
			all = append(all, kv{k, v.last})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, e := range all[:1000] {
			delete(l.ips, e.ip)
		}
	}
}

// Success forgets an IP's failures.
func (l *RateLimiter) Success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

// Tracked is how many IPs have recorded failures (for tests).
func (l *RateLimiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ips)
}
