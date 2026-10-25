package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// key is testutil.Key, repeated here: testutil imports this package.
const key = "fakefakefakefakefakefakefakefake"

type seen struct {
	method, path string
	header       http.Header
	body         string
}

type fakeApp struct {
	mu   sync.Mutex
	reqs []seen
}

func (a *fakeApp) last() seen {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reqs[len(a.reqs)-1]
}

func (a *fakeApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.reqs = append(a.reqs, seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), string(body)})
	a.mu.Unlock()
	send := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if b, ok := v.([]byte); ok {
			w.Write(b)
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	if r.Header.Get("X-Api-Key") != key {
		send(401, map[string]any{"error": "unauthorized"})
		return
	}
	if r.Method == http.MethodPost {
		send(201, map[string]any{"id": 42, "name": "AlbumSearch"})
		return
	}
	p := r.URL.RequestURI()
	switch {
	case strings.HasPrefix(p, "/api/v1/wanted/missing"):
		send(200, map[string]any{"records": []any{map[string]any{"id": 1}}, "sortKey": "albums.lastSearchTime", "totalRecords": 12})
	case p == "/api/v1/system/status":
		send(200, map[string]any{"appName": "Lidarr", "version": "3.1"})
	case p == "/api/v1/health":
		send(200, []any{map[string]any{"source": "UpdateCheck"}})
	case p == "/api/v1/queue?page=1&pageSize=1":
		send(200, map[string]any{"totalRecords": 7})
	case p == "/api/v1/qualityprofile":
		send(200, []any{map[string]any{"id": 1, "upgradeAllowed": true}, map[string]any{"id": 2, "upgradeAllowed": false}})
	case p == "/api/v1/redirect":
		w.Header().Set("Location", "http://evil.example/steal")
		w.WriteHeader(302)
	case p == "/api/v1/invalid":
		send(400, []any{map[string]any{"propertyName": "Path", "errorMessage": "Folder does not exist"}})
	case p == "/api/v1/problem":
		send(400, map[string]any{"title": "One or more validation errors occurred.", "errors": map[string]any{"$.addOptions.monitor": []any{"could not be converted"}}})
	case p == "/api/v1/message":
		send(409, map[string]any{"message": "already exists"})
	case p == "/api/v1/servererror":
		send(500, map[string]any{"message": "stack trace with internals"})
	case p == "/api/v1/html":
		w.WriteHeader(200)
		w.Write([]byte("<html>login</html>"))
	case p == "/api/v1/huge":
		send(200, []byte("["+strings.Repeat("1,", 600)+"1]"))
	case p == "/api/v1/empty":
		w.WriteHeader(200)
	case p == "/api/v1/slow":
		time.Sleep(300 * time.Millisecond)
		send(200, []any{})
	default:
		send(404, map[string]any{"error": "nope"})
	}
}

func setup(t *testing.T) (*Client, *fakeApp) {
	t.Helper()
	app := &fakeApp{}
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	c, err := New("lidarr", srv.URL, key, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c, app
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("want error containing %q, got %v", substr, err)
	}
}

func TestEndpoints(t *testing.T) {
	c, app := setup(t)
	if s, err := c.Status(); err != nil || s["appName"] != "Lidarr" {
		t.Fatal(s, err)
	}
	if h, err := c.Health(); err != nil || len(h) != 1 || h[0]["source"] != "UpdateCheck" {
		t.Fatal(h, err)
	}
	if q, err := c.QueueSize(); err != nil || q != 7 {
		t.Fatal(q, err)
	}
	if p, err := c.UpgradeProfiles(); err != nil || len(p) != 1 || !p[1] {
		t.Fatal(p, err)
	}
	page, err := c.Wanted("missing", 5)
	if err != nil || len(page.Records) != 1 || page.SortKey != "albums.lastSearchTime" || page.Total != 12 {
		t.Fatal(page, err)
	}
	p := app.last().path
	for _, want := range []string{"sortKey=albums.lastSearchTime", "includeArtist=true", "monitored=true", "pageSize=5"} {
		if !strings.Contains(p, want) {
			t.Errorf("%s missing from %s", want, p)
		}
	}
}

func TestSendsKeyHeaderAndUserAgent(t *testing.T) {
	c, app := setup(t)
	c.Status()
	h := app.last().header
	if h.Get("X-Api-Key") != key || !strings.HasPrefix(h.Get("User-Agent"), "arr-trickle/") {
		t.Fatal(h)
	}
}

func TestCommandPostsJSON(t *testing.T) {
	c, app := setup(t)
	res, err := c.Command(map[string]any{"name": "AlbumSearch", "albumIds": []int{1}})
	if err != nil || res["id"] != 42.0 {
		t.Fatal(res, err)
	}
	if got := app.last(); got.method != "POST" || got.path != "/api/v1/command" || got.body != `{"albumIds":[1],"name":"AlbumSearch"}` {
		t.Fatal(got)
	}
}

func TestBadKeyIsExplainedWithoutLeaking(t *testing.T) {
	_, app := setup(t)
	srv := httptest.NewServer(app)
	defer srv.Close()
	bad := strings.Repeat("wrongkey", 4)
	c, _ := New("lidarr", srv.URL, bad, 5*time.Second)
	_, err := c.Status()
	wantErr(t, err, "401")
	wantErr(t, err, "check the API key")
	if strings.Contains(err.Error(), "wrongkey") {
		t.Fatal(err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	c, app := setup(t)
	_, err := c.Request("GET", "/redirect", nil)
	wantErr(t, err, "302")
	if len(app.reqs) != 1 {
		t.Fatal("the redirect was followed")
	}
}

func TestNonJSONExplained(t *testing.T) {
	c, _ := setup(t)
	_, err := c.Request("GET", "/html", nil)
	wantErr(t, err, "not JSON")
}

func TestEmptyBodyIsNil(t *testing.T) {
	c, _ := setup(t)
	if v, err := c.Request("GET", "/empty", nil); v != nil || err != nil {
		t.Fatal(v, err)
	}
}

func TestOversizedResponseRefused(t *testing.T) {
	c, _ := setup(t)
	old := MaxResponseBytes
	MaxResponseBytes = 100
	defer func() { MaxResponseBytes = old }()
	_, err := c.Request("GET", "/huge", nil)
	wantErr(t, err, "larger than")
}

func TestUnreachableAndTimeout(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	c, _ := New("radarr", "http://"+addr, key, 2*time.Second)
	_, err := c.Status()
	wantErr(t, err, "GET /system/status")
	slow, _ := setup(t)
	slow.http.Timeout = 50 * time.Millisecond
	_, err = slow.Request("GET", "/slow", nil)
	wantErr(t, err, "timed out")
}

func TestStringHidesKey(t *testing.T) {
	c, _ := setup(t)
	for _, s := range []string{c.String(), fmt.Sprintf("%v %+v %#v %s", c, c, c, c)} {
		if strings.Contains(s, key) {
			t.Fatal(s)
		}
	}
}

func TestRejectsNonHTTPScheme(t *testing.T) {
	_, err := New("radarr", "file:///etc/passwd", key, time.Second)
	wantErr(t, err, "scheme")
}

func TestValidationErrorsAreReported(t *testing.T) {
	c, _ := setup(t)
	for path, msg := range map[string]string{
		"/invalid": "Path: Folder does not exist", "/problem": "$.addOptions.monitor: could not be converted",
		"/message": "already exists", "/servererror": "HTTP 500",
	} {
		_, err := c.Request("GET", path, nil)
		wantErr(t, err, msg)
	}
	_, err := c.Request("GET", "/servererror", nil)
	if strings.Contains(err.Error(), "internals") {
		t.Fatal(err)
	}
}

func TestUnexpectedShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`"x"`)) }))
	defer srv.Close()
	c, _ := New("sonarr", srv.URL, key, time.Second)
	for name, call := range map[string]func() error{
		"status":   func() error { _, err := c.Status(); return err },
		"health":   func() error { _, err := c.Health(); return err },
		"queue":    func() error { _, err := c.QueueSize(); return err },
		"profiles": func() error { _, err := c.UpgradeProfiles(); return err },
		"wanted":   func() error { _, err := c.Wanted("missing", 1); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "unexpected response shape") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
