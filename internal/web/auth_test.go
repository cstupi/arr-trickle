package web

import (
	"encoding/json"
	"fmt"
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
	h := HashPassword(pw)
	raw, _ := json.Marshal(h)
	want(t, h.Alg == "scrypt" && h.N >= 1<<14 && len(h.Salt) >= 22 && !strings.Contains(string(raw), pw), "%+v", h)
	want(t, VerifyPassword(pw, h) && !VerifyPassword(pw+"x", h), "verify")
	want(t, HashPassword(pw).Salt != h.Salt, "random salt")
	want(t, !VerifyPassword(pw, PasswordHash{Salt: "!!", Hash: "", N: 2, R: 1, P: 1}) && !VerifyPassword(pw, PasswordHash{}), "bad records")
	want(t, !VerifyPassword(pw, PasswordHash{Salt: h.Salt, Hash: h.Hash, N: 1 << 30, R: 8, P: 1}), "absurd cost refused")
}

func TestReadsHashesWrittenByThePythonVersion(t *testing.T) {
	// Computed with Python's hashlib.scrypt, as arr-trickle's Python version stored it, for the password "correct horse battery".
	h := PasswordHash{Alg: "scrypt", N: 16384, R: 8, P: 1, Salt: "c2FsdHNhbHRzYWx0c2FsdA==",
		Hash: "LPDRs+d7MJ+D4v778lUnSsMuTt3eMzZtYlcDVF6agWU="}
	want(t, VerifyPassword(pw, h) && !VerifyPassword(pw+"!", h), "a 0.3 (Python) hash must verify, so upgrades keep the login")
}

func TestSetupCodeCreatesTheSingleAdmin(t *testing.T) {
	dir := t.TempDir()
	s := NewAuthStore(dir)
	_, ok := s.CredentialID()
	want(t, !s.HasAdmin() && !ok, "no admin")
	code, err := s.NewSetupCode()
	want(t, err == nil && len(code) == 23 && strings.Count(code, "-") == 3, "%s", code)
	info, _ := os.Stat(s.CodePath)
	raw, _ := os.ReadFile(s.CodePath)
	want(t, info.Mode().Perm() == 0o600 && !strings.Contains(string(raw), code), "only the hash is stored")
	want(t, s.CheckSetupCode(strings.ToLower(strings.ReplaceAll(code, "-", " "))), "forgiving about case and separators")
	want(t, s.CreateAdmin(code, " admin ", pw, pw) == nil, "create")
	cred, ok := s.CredentialID()
	info, _ = os.Stat(s.Path)
	raw, _ = os.ReadFile(s.Path)
	want(t, s.HasAdmin() && ok && cred != "" && info.Mode().Perm() == 0o600 && !strings.Contains(string(raw), pw), "admin stored")
	_, err = os.Stat(s.CodePath)
	want(t, os.IsNotExist(err), "single use")
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
	cred, ok := s.VerifyLogin("admin", pw)
	stored, _ := s.CredentialID()
	want(t, ok && cred == stored, "login")
	_, bad1 := s.VerifyLogin("admin", "wrong password!")
	_, bad2 := s.VerifyLogin("root", pw)
	want(t, !bad1 && !bad2, "wrong credentials")
	code, err := s.Reset()
	_, after := s.VerifyLogin("admin", pw)
	want(t, err == nil && !s.HasAdmin() && !after, "reset")
	s.CreateAdmin(code, "admin", pw, pw)
	again, _ := s.CredentialID()
	want(t, again != cred, "old sessions are invalidated")
	_, long := s.VerifyLogin("admin", strings.Repeat("x", 5000))
	want(t, !long, "long password")
}

func TestLoginWithoutAdminFails(t *testing.T) {
	_, ok := NewAuthStore(t.TempDir()).VerifyLogin("", "")
	want(t, !ok, "no admin: nobody logs in")
}

func TestCorruptAuthFileMeansNoAdmin(t *testing.T) {
	s := NewAuthStore(t.TempDir())
	os.WriteFile(s.Path, []byte("{nope"), 0o600)
	want(t, !s.HasAdmin(), "corrupt")
	os.WriteFile(s.Path, []byte(`{"username": 1}`), 0o600)
	want(t, !s.HasAdmin(), "wrong types")
}

func TestSessionsExpireIdleAndAbsolute(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewSessions(2*time.Hour, time.Hour)
	s.Clock = func() time.Time { return now }
	x := s.Create("cred")
	want(t, len(x.ID) >= 43 && x.CSRF != x.ID, "32 random bytes")
	now = now.Add(50 * time.Minute)
	want(t, s.Get(x.ID) == x, "alive")
	now = now.Add(50 * time.Minute)
	want(t, s.Get(x.ID) == x, "get refreshed the idle timer")
	now = now.Add(61 * time.Minute)
	want(t, s.Get(x.ID) == nil, "idle timeout")
	x = s.Create("cred")
	for range 4 {
		now = now.Add(32 * time.Minute)
		s.Get(x.ID)
	}
	want(t, s.Get(x.ID) == nil, "absolute timeout despite activity")
	want(t, s.Get("") == nil && s.Get("forged") == nil, "unknown ids")
}

func TestAnonymousSessionsAreCappedAndShortLived(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewSessions(24*time.Hour, time.Hour)
	s.Clock = func() time.Time { return now }
	s.MaxAnon = 10
	authed := s.Create("cred")
	var anon []*Session
	for range 30 {
		now = now.Add(time.Millisecond)
		anon = append(anon, s.Create(""))
	}
	want(t, s.Get(authed.ID) == authed, "logged-in sessions are kept")
	want(t, s.Get(anon[0].ID) == nil && s.Get(anon[29].ID) == anon[29], "oldest dropped")
	now = now.Add(s.AnonLifetime + time.Second)
	want(t, s.Get(anon[29].ID) == nil, "short-lived")
}

func TestFlashMessages(t *testing.T) {
	s := NewSessions(time.Hour, time.Hour)
	x := s.Create("")
	s.AddFlash(x, "hi")
	want(t, fmt.Sprint(s.TakeFlash(x)) == "[hi]" && s.TakeFlash(x) == nil, "flash")
}

func TestRateLimiterBacksOffPerIP(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewRateLimiter()
	l.Free, l.MaxBackoff, l.Clock = 3, time.Minute, func() time.Time { return now }
	for range 3 {
		want(t, l.RetryAfter("a") == 0, "free attempts")
		l.Failure("a")
	}
	want(t, l.RetryAfter("a") == time.Second, "%v", l.RetryAfter("a"))
	l.Failure("a")
	want(t, l.RetryAfter("a") == 2*time.Second && l.RetryAfter("b") == 0, "doubling, per IP")
	for range 20 {
		l.Failure("a")
	}
	want(t, l.RetryAfter("a") == time.Minute, "capped")
	now = now.Add(61 * time.Second)
	want(t, l.RetryAfter("a") == 0, "expired")
	now = now.Add(200 * time.Second)
	l.Failure("a") // a long quiet period: counting starts over
	want(t, l.RetryAfter("a") == 0, "reset")
	l.Success("a")
	want(t, l.Tracked() == 0, "success forgets")
}

func TestRateLimiterGlobalCeiling(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewRateLimiter()
	l.Free, l.GlobalLimit, l.GlobalWindow, l.Clock = 100, 5, time.Minute, func() time.Time { return now }
	for i := range 5 {
		l.Failure(fmt.Sprintf("10.0.0.%d", i))
	}
	want(t, l.RetryAfter("10.0.0.99") == time.Minute, "global")
	now = now.Add(61 * time.Second)
	want(t, l.RetryAfter("10.0.0.99") == 0, "window passed")
}

func TestRateLimiterMemoryIsBounded(t *testing.T) {
	l := NewRateLimiter()
	l.GlobalLimit = 1 << 30
	for i := range 10_001 {
		l.Failure(fmt.Sprintf("ip%d", i))
	}
	want(t, l.Tracked() <= 10_000, "%d", l.Tracked())
}

func TestClientIPAndProxies(t *testing.T) {
	trusted := Networks([]string{"10.0.0.2/32"})
	ip := func(peer, xff string) string {
		a, ok := ClientIP(peer, xff, trusted)
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
	a, _ := ParseIP("::ffff:192.168.1.5")
	b, _ := ParseIP("fe80::1%eth0")
	_, empty := ParseIP("")
	want(t, a.String() == "192.168.1.5" && b.String() == "fe80::1" && !empty, "parse")
}

func TestCodeAlphabetIsUnambiguous(t *testing.T) {
	want(t, !strings.ContainsAny(codeAlphabet, "01IO") && 256%len(codeAlphabet) == 0, "alphabet")
}
