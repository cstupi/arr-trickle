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
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"

	"github.com/cstupi/arr-trickle/internal/state"
)

// Password rules and scrypt cost (~32 MiB and tens of milliseconds per hash). The cost is stored with
// each hash, so it can be raised later.
const (
	minPassword = 12
	maxPassword = 1024
	ScryptN     = 1 << 15
)

// scryptN is ScryptN, lowered only by tests (hashing at full cost under the race detector is slow).
var scryptN = ScryptN

// hashSlots caps concurrent password hashes, so a burst of logins can't exhaust memory.
var hashSlots = make(chan struct{}, 2)

// authError is a setup or password problem; the message is safe to show.
type authError string

func (e authError) Error() string { return string(e) }

// passwordHash is the stored form of a password (the same JSON arr-trickle's Python version wrote).
type passwordHash struct {
	Alg  string `json:"alg"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

func hashPassword(password string) passwordHash {
	salt := make([]byte, 16)
	rand.Read(salt)
	dk, err := scrypt.Key([]byte(password), salt, scryptN, 8, 1, 32)
	if err != nil {
		panic(err) // only for invalid constant parameters
	}
	enc := base64.StdEncoding.EncodeToString
	return passwordHash{"scrypt", scryptN, 8, 1, enc(salt), enc(dk)}
}

// verifyPassword compares in constant time.
func verifyPassword(password string, h passwordHash) bool {
	salt, err1 := base64.StdEncoding.DecodeString(h.Salt)
	expected, err2 := base64.StdEncoding.DecodeString(h.Hash)
	if err1 != nil || err2 != nil || len(expected) == 0 || h.N > 1<<20 || h.R > 32 || h.P > 16 {
		return false
	}
	dk, err := scrypt.Key([]byte(password), salt, h.N, h.R, h.P, len(expected))
	return err == nil && hmac.Equal(dk, expected)
}

func checkNewPassword(password, confirm string) error {
	switch n := utf8.RuneCountInString(password); {
	case n < minPassword:
		return authError(fmt.Sprintf("the password must be at least %d characters", minPassword))
	case n > maxPassword:
		return authError(fmt.Sprintf("the password must be at most %d characters", maxPassword))
	case !hmac.Equal([]byte(password), []byte(confirm)):
		return authError("the passwords do not match")
	}
	return nil
}

// codeDigest: the code has 100 random bits, so a plain hash is enough (no need for a slow KDF).
func codeDigest(code string) string {
	norm := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToUpper(r)
		}
		return -1
	}, code)
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// A hash compared against when there is no admin, so a login takes the same time either way.
var dummy = sync.OnceValue(func() passwordHash { return hashPassword(rand.Text()) })

type admin struct {
	Version      int          `json:"version"`
	Username     string       `json:"username"`
	Password     passwordHash `json:"password"`
	CredentialID string       `json:"credential_id"`
}

// AuthStore is auth.json (the admin) and setup-code (hash of the one-time code). Both are re-read on
// every use, so the `reset-password` command takes effect in a running server immediately.
type AuthStore struct{ path, codePath string }

// NewAuthStore uses files in stateDir.
func NewAuthStore(stateDir string) *AuthStore {
	return &AuthStore{filepath.Join(stateDir, "auth.json"), filepath.Join(stateDir, "setup-code")}
}

func (s *AuthStore) admin() *admin {
	var a admin
	raw, err := os.ReadFile(s.path)
	if err != nil || json.Unmarshal(raw, &a) != nil || a.Username == "" || a.Password.Hash == "" {
		return nil
	}
	return &a
}

// HasAdmin reports whether the admin account exists.
func (s *AuthStore) HasAdmin() bool { return s.admin() != nil }

// credentialID changes whenever the admin is (re)created; sessions from before a reset stop working.
func (s *AuthStore) credentialID() (string, bool) {
	if a := s.admin(); a != nil {
		return a.CredentialID, true
	}
	return "", false
}

// NewSetupCode makes a fresh one-time code (20 base32 characters in groups of 5), stores its hash,
// and returns it.
func (s *AuthStore) NewSetupCode() (string, error) {
	t := rand.Text()
	code := strings.Join([]string{t[0:5], t[5:10], t[10:15], t[15:20]}, "-")
	return code, state.AtomicWrite(s.codePath, []byte(codeDigest(code)), 0o600)
}

// CheckSetupCode compares in constant time.
func (s *AuthStore) CheckSetupCode(code string) bool {
	raw, err := os.ReadFile(s.codePath)
	expected := strings.TrimSpace(string(raw))
	return err == nil && expected != "" && hmac.Equal([]byte(codeDigest(code)), []byte(expected))
}

// CreateAdmin creates the single admin account with a valid setup code.
func (s *AuthStore) CreateAdmin(code, username, password, confirm string) error {
	username = strings.TrimSpace(username)
	switch n := utf8.RuneCountInString(username); {
	case s.HasAdmin():
		return authError("an admin already exists")
	case !s.CheckSetupCode(code):
		return authError("the setup code is not valid")
	case n < 1 || n > 64:
		return authError("the username must be 1-64 characters")
	}
	if err := checkNewPassword(password, confirm); err != nil {
		return err
	}
	raw, _ := json.Marshal(admin{1, username, hashPassword(password), rand.Text()})
	if err := state.AtomicWrite(s.path, raw, 0o600); err != nil {
		return err
	}
	return removeIfExists(s.codePath)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// verifyLogin returns the credential id on success. It always does the full hash, so timing doesn't
// reveal whether the username exists.
func (s *AuthStore) verifyLogin(username, password string) (string, bool) {
	a := s.admin()
	h, name := dummy(), ""
	if a != nil {
		h, name = a.Password, a.Username
	}
	if r := []rune(password); len(r) > maxPassword {
		password = string(r[:maxPassword])
	}
	ok := verifyPassword(password, h)
	nameOK := hmac.Equal([]byte(strings.TrimSpace(username)), []byte(name))
	if a != nil && ok && nameOK {
		return a.CredentialID, true
	}
	return "", false
}

// Reset removes the admin (ending every session) and returns a fresh setup code.
func (s *AuthStore) Reset() (string, error) {
	if err := removeIfExists(s.path); err != nil {
		return "", err
	}
	return s.NewSetupCode()
}

// session is server-side; the cookie holds only its random id.
type session struct {
	id, csrf          string
	created, lastSeen time.Time
	credentialID      string // "" = not logged in yet (login/setup forms only)
	flash             []string
}

func (s *session) authenticated() bool { return s.credentialID != "" }

// token32 is 32 random bytes, URL-safe.
func token32() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// sessions holds every session in memory; a restart logs everyone out.
type sessions struct {
	absolute, idle, anonLifetime time.Duration
	maxAnon                      int
	clock                        func() time.Time
	mu                           sync.Mutex
	byID                         map[string]*session
}

func newSessions(absolute, idle time.Duration) *sessions {
	return &sessions{absolute: absolute, idle: min(idle, absolute), anonLifetime: time.Hour, maxAnon: 1000,
		clock: time.Now, byID: map[string]*session{}}
}

func (s *sessions) expired(x *session, now time.Time) bool {
	if x.authenticated() {
		return now.Sub(x.created) > s.absolute || now.Sub(x.lastSeen) > s.idle
	}
	return now.Sub(x.created) > s.anonLifetime
}

// create starts a session; credentialID "" makes a not-logged-in one.
func (s *sessions) create(credentialID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	maps.DeleteFunc(s.byID, func(_ string, x *session) bool { return s.expired(x, now) })
	var anon []*session
	for _, x := range s.byID {
		if !x.authenticated() {
			anon = append(anon, x)
		}
	}
	if extra := len(anon) - s.maxAnon + 1; extra > 0 { // bound memory: drop the oldest not-logged-in sessions
		slices.SortFunc(anon, func(a, b *session) int { return a.created.Compare(b.created) })
		for _, x := range anon[:extra] {
			delete(s.byID, x.id)
		}
	}
	x := &session{id: token32(), csrf: token32(), created: now, lastSeen: now, credentialID: credentialID}
	s.byID[x.id] = x
	return x
}

// get returns a live session and refreshes its idle timer.
func (s *sessions) get(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.byID[id]
	if x == nil {
		return nil
	}
	if now := s.clock(); !s.expired(x, now) {
		x.lastSeen = now
		return x
	}
	delete(s.byID, id)
	return nil
}

func (s *sessions) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *sessions) addFlash(x *session, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x.flash = append(x.flash, msg)
}

func (s *sessions) takeFlash(x *session) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := x.flash
	x.flash = nil
	return out
}

// rateLimiter throttles failed logins: exponential backoff per client after a few free attempts,
// plus a global ceiling so an attacker rotating addresses is slowed too. IPv6 clients are counted
// per /64, since one host can use a whole /64.
type rateLimiter struct {
	free, globalLimit        int
	maxBackoff, globalWindow time.Duration
	clock                    func() time.Time
	mu                       sync.Mutex
	clients                  map[string]failures
	recent                   []time.Time
}

type failures struct {
	n    int
	last time.Time
}

// newRateLimiter has the production defaults: 5 free attempts, backoff up to 15 minutes, 30 failures per 10 minutes.
func newRateLimiter() *rateLimiter {
	return &rateLimiter{free: 5, globalLimit: 30, maxBackoff: 15 * time.Minute, globalWindow: 10 * time.Minute,
		clock: time.Now, clients: map[string]failures{}}
}

func clientKey(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return ip
}

// retryAfter is how long this client must wait (0 = allowed now).
func (l *rateLimiter) retryAfter(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	l.recent = slices.DeleteFunc(l.recent, func(t time.Time) bool { return now.Sub(t) > l.globalWindow })
	var wait time.Duration
	if len(l.recent) >= l.globalLimit {
		wait = l.recent[0].Add(l.globalWindow).Sub(now)
	}
	if f := l.clients[clientKey(ip)]; f.n >= l.free {
		wait = max(wait, f.last.Add(min(l.maxBackoff, time.Second<<min(f.n-l.free, 30))).Sub(now))
	}
	return max(wait, 0)
}

func (l *rateLimiter) failure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now, key := l.clock(), clientKey(ip)
	f := l.clients[key]
	if f.n > 0 && now.Sub(f.last) > 2*l.maxBackoff {
		f.n = 0 // quiet for a while: start over
	}
	l.clients[key] = failures{f.n + 1, now}
	l.recent = append(l.recent, now)
	if len(l.clients) > 10_000 { // bound memory: drop the stalest entries
		keys := slices.SortedFunc(maps.Keys(l.clients), func(a, b string) int { return l.clients[a].last.Compare(l.clients[b].last) })
		for _, k := range keys[:1000] {
			delete(l.clients, k)
		}
	}
}

func (l *rateLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, clientKey(ip))
}
