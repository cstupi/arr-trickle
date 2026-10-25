package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

const pw = "correct horse battery"

func TestScryptHashAndVerify(t *testing.T) {
	scryptN = ScryptN
	defer func() { scryptN = 1 << 10 }()
	want(t, ScryptN >= 1<<15, "production cost")
	h := hashPassword(pw)
	raw, _ := json.Marshal(h)
	want(t, h.Alg == "scrypt" && h.N >= 1<<14 && len(h.Salt) >= 22 && !strings.Contains(string(raw), pw), "%+v", h)
	want(t, verifyPassword(pw, h) && !verifyPassword(pw+"x", h), "verify")
	want(t, hashPassword(pw).Salt != h.Salt, "random salt")
	want(t, !verifyPassword(pw, passwordHash{Salt: "!!", Hash: "", N: 2, R: 1, P: 1}) && !verifyPassword(pw, passwordHash{}), "bad records")
	want(t, !verifyPassword(pw, passwordHash{Salt: h.Salt, Hash: h.Hash, N: 1 << 30, R: 8, P: 1}), "absurd cost refused")
}

func TestReadsHashesWrittenByThePythonVersion(t *testing.T) {
	// Computed with Python's hashlib.scrypt, as arr-trickle's Python version stored it, for the password "correct horse battery".
	h := passwordHash{Alg: "scrypt", N: 16384, R: 8, P: 1, Salt: "c2FsdHNhbHRzYWx0c2FsdA==",
		Hash: "LPDRs+d7MJ+D4v778lUnSsMuTt3eMzZtYlcDVF6agWU="}
	want(t, verifyPassword(pw, h) && !verifyPassword(pw+"!", h), "a 0.3 (Python) hash must verify, so upgrades keep the login")
}

func TestSetupCodeCreatesTheSingleAdmin(t *testing.T) {
	dir := t.TempDir()
	s := NewAuthStore(dir)
	_, ok := s.credentialID()
	want(t, !s.HasAdmin() && !ok, "no admin")
	code, err := s.NewSetupCode()
	want(t, err == nil && len(code) == 23 && strings.Count(code, "-") == 3, "%s", code)
	info, _ := os.Stat(s.codePath)
	raw, _ := os.ReadFile(s.codePath)
	want(t, info.Mode().Perm() == 0o600 && !strings.Contains(string(raw), code), "only the hash is stored")
	want(t, s.CheckSetupCode(strings.ToLower(strings.ReplaceAll(code, "-", " "))), "forgiving about case and separators")
	want(t, s.CreateAdmin(code, " admin ", pw, pw) == nil, "create")
	cred, ok := s.credentialID()
	info, _ = os.Stat(s.path)
	raw, _ = os.ReadFile(s.path)
	want(t, s.HasAdmin() && ok && cred != "" && info.Mode().Perm() == 0o600 && !strings.Contains(string(raw), pw), "admin stored")
	_, err = os.Stat(s.codePath)
	want(t, errors.Is(err, fs.ErrNotExist), "single use")
	err = s.CreateAdmin(code, "admin", pw, pw)
	want(t, err != nil && strings.Contains(err.Error(), "already exists"), "%v", err)
}

func TestSetupRejects(t *testing.T) {
	cases := []struct {
		codeOK                   bool
		user, pass, confirm, msg string
	}{
		{false, "admin", pw, pw, "setup code"},
		{true, "", pw, pw, "username"},
		{true, "admin", "short", "short", "at least 12"},
		{true, "admin", strings.Repeat("x", 2000), strings.Repeat("x", 2000), "at most"},
		{true, "admin", pw, pw + "!", "do not match"},
	}
	for _, c := range cases {
		s := NewAuthStore(t.TempDir())
		code, _ := s.NewSetupCode()
		if !c.codeOK {
			code = "WRONG-CODE"
		}
		err := s.CreateAdmin(code, c.user, c.pass, c.confirm)
		want(t, err != nil && strings.Contains(err.Error(), c.msg) && !s.HasAdmin(), "%s: %v", c.msg, err)
	}
}

func TestNoCodeFileMeansNoSetup(t *testing.T) {
	s := NewAuthStore(t.TempDir())
	want(t, !s.CheckSetupCode("") && !s.CheckSetupCode("ANYTHING"), "no code")
}

func TestLoginAndReset(t *testing.T) {
	s := NewAuthStore(t.TempDir())
	code, _ := s.NewSetupCode()
	s.CreateAdmin(code, "admin", pw, pw)
	cred, ok := s.verifyLogin("admin", pw)
	stored, _ := s.credentialID()
	want(t, ok && cred == stored, "login")
	_, bad1 := s.verifyLogin("admin", "wrong password!")
	_, bad2 := s.verifyLogin("root", pw)
	want(t, !bad1 && !bad2, "wrong credentials")
	code, err := s.Reset()
	_, after := s.verifyLogin("admin", pw)
	want(t, err == nil && !s.HasAdmin() && !after, "reset")
	s.CreateAdmin(code, "admin", pw, pw)
	again, _ := s.credentialID()
	want(t, again != cred, "old sessions are invalidated")
	_, long := s.verifyLogin("admin", strings.Repeat("x", 5000))
	want(t, !long, "long password")
}

func TestLoginWithoutAdminFails(t *testing.T) {
	_, ok := NewAuthStore(t.TempDir()).verifyLogin("", "")
	want(t, !ok, "no admin: nobody logs in")
}

func TestCorruptAuthFileMeansNoAdmin(t *testing.T) {
	s := NewAuthStore(t.TempDir())
	os.WriteFile(s.path, []byte("{nope"), 0o600)
	want(t, !s.HasAdmin(), "corrupt")
	os.WriteFile(s.path, []byte(`{"username": 1}`), 0o600)
	want(t, !s.HasAdmin(), "wrong types")
}

func TestSessionsExpireIdleAndAbsolute(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newSessions(2*time.Hour, time.Hour)
	s.clock = func() time.Time { return now }
	x := s.create("cred")
	want(t, len(x.id) >= 43 && x.csrf != x.id, "32 random bytes")
	now = now.Add(50 * time.Minute)
	want(t, s.get(x.id) == x, "alive")
	now = now.Add(50 * time.Minute)
	want(t, s.get(x.id) == x, "get refreshed the idle timer")
	now = now.Add(61 * time.Minute)
	want(t, s.get(x.id) == nil, "idle timeout")
	x = s.create("cred")
	for range 4 {
		now = now.Add(32 * time.Minute)
		s.get(x.id)
	}
	want(t, s.get(x.id) == nil, "absolute timeout despite activity")
	want(t, s.get("") == nil && s.get("forged") == nil, "unknown ids")
}

func TestAnonymousSessionsAreCappedAndShortLived(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newSessions(24*time.Hour, time.Hour)
	s.clock = func() time.Time { return now }
	s.maxAnon = 10
	authed := s.create("cred")
	var anon []*session
	for range 30 {
		now = now.Add(time.Millisecond)
		anon = append(anon, s.create(""))
	}
	want(t, s.get(authed.id) == authed, "logged-in sessions are kept")
	want(t, s.get(anon[0].id) == nil && s.get(anon[29].id) == anon[29], "oldest dropped")
	now = now.Add(s.anonLifetime + time.Second)
	want(t, s.get(anon[29].id) == nil, "short-lived")
}

func TestFlashMessages(t *testing.T) {
	s := newSessions(time.Hour, time.Hour)
	x := s.create("")
	s.addFlash(x, "hi")
	want(t, fmt.Sprint(s.takeFlash(x)) == "[hi]" && s.takeFlash(x) == nil, "flash")
}

func TestRateLimiterBacksOffPerIP(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter()
	l.free, l.maxBackoff, l.clock = 3, time.Minute, func() time.Time { return now }
	for range 3 {
		want(t, l.retryAfter("a") == 0, "free attempts")
		l.failure("a")
	}
	want(t, l.retryAfter("a") == time.Second, "%v", l.retryAfter("a"))
	l.failure("a")
	want(t, l.retryAfter("a") == 2*time.Second && l.retryAfter("b") == 0, "doubling, per IP")
	for range 20 {
		l.failure("a")
	}
	want(t, l.retryAfter("a") == time.Minute, "capped")
	now = now.Add(61 * time.Second)
	want(t, l.retryAfter("a") == 0, "expired")
	now = now.Add(200 * time.Second)
	l.failure("a") // a long quiet period: counting starts over
	want(t, l.retryAfter("a") == 0, "reset")
	l.success("a")
	want(t, len(l.clients) == 0, "success forgets")
}

func TestRateLimiterGlobalCeiling(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter()
	l.free, l.globalLimit, l.globalWindow, l.clock = 100, 5, time.Minute, func() time.Time { return now }
	for i := range 5 {
		l.failure(fmt.Sprintf("10.0.0.%d", i))
	}
	want(t, l.retryAfter("10.0.0.99") == time.Minute, "global")
	now = now.Add(61 * time.Second)
	want(t, l.retryAfter("10.0.0.99") == 0, "window passed")
}

func TestRateLimiterMemoryIsBounded(t *testing.T) {
	l := newRateLimiter()
	l.globalLimit = 1 << 30
	for i := range 10_001 {
		l.failure(fmt.Sprintf("ip%d", i))
	}
	want(t, len(l.clients) <= 10_000, "%d", len(l.clients))
}

func TestClientIPAndProxies(t *testing.T) {
	trusted := networks([]string{"10.0.0.2/32"})
	ip := func(peer, xff string) string {
		a, ok := clientIP(peer, xff, trusted)
		if !ok {
			return "deny"
		}
		return a.String()
	}
	want(t, ip("192.168.1.5", "") == "192.168.1.5", "plain")
	want(t, ip("192.168.1.5", "8.8.8.8") == "192.168.1.5", "untrusted peer: XFF ignored")
	want(t, ip("10.0.0.2", "8.8.8.8") == "8.8.8.8", "trusted proxy")
	want(t, ip("10.0.0.2", "1.1.1.1, 192.168.1.9") == "192.168.1.9", "rightmost untrusted")
	want(t, ip("10.0.0.2", "192.168.1.9, 10.0.0.2") == "192.168.1.9", "skips trusted hops")
	want(t, ip("10.0.0.2", "garbage") == "deny" && ip("testclient", "") == "deny", "malformed")
	a, _ := parseIP("::ffff:192.168.1.5")
	b, _ := parseIP("fe80::1%eth0")
	_, empty := parseIP("")
	want(t, a.String() == "192.168.1.5" && b.String() == "fe80::1" && !empty, "parse")
}
