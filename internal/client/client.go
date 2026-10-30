// Package client is a minimal *arr REST client (standard library only).
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/version"
)

// MaxResponseBytes caps a response: wanted pages are small; anything far larger means something is wrong (or hostile).
var MaxResponseBytes int64 = 16 * 1024 * 1024

// Error is a failed request. The message never includes the API key.
type Error struct {
	Msg    string
	Status int  // the HTTP status, or 0 when there was no HTTP answer
	NoConn bool // the app never answered: connection refused, DNS, timeout
}

func (e *Error) Error() string { return e.Msg }

func errorf(format string, args ...any) *Error { return &Error{Msg: fmt.Sprintf(format, args...)} }

func httpErrorf(status int, format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Status: status}
}

// Unreachable reports whether err means the app never answered (as opposed to answering with an error).
func Unreachable(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.NoConn
}

// CommandRetryDelays are the waits before re-sending a /command the app answered with a 5xx, e.g. Lidarr's
// "database is locked" while another write holds its SQLite file. A 5xx there means the command was not queued.
var CommandRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}

// WantedPage is one page of /wanted/{missing,cutoff}: the records, the sort key the app echoed back, and totalRecords.
type WantedPage struct {
	Records []apps.Record
	SortKey string
	Total   int
}

// Client talks to one *arr app.
type Client struct {
	Spec   apps.Spec
	Base   string
	apiKey string
	http   *http.Client
}

// New checks the URL scheme and builds a client. Redirects are never followed: that would resend
// X-Api-Key to wherever the app (or a proxy) points.
func New(kind, rawURL, apiKey string, timeout time.Duration) (*Client, error) {
	if u, err := url.Parse(rawURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errorf("unsupported URL scheme in '%s'", rawURL)
	}
	spec := apps.Specs[kind]
	return &Client{
		Spec:   spec,
		Base:   strings.TrimRight(rawURL, "/") + "/api/" + spec.APIVersion,
		apiKey: apiKey,
		http: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// String never shows the key.
func (c *Client) String() string { return fmt.Sprintf("Client(%q, %q)", c.Spec.Kind, c.Base) }

// GoString never shows the key either (%#v).
func (c *Client) GoString() string { return c.String() }

// errorDetail is the app's own validation messages for a 4xx, e.g. ": Path: Folder does not exist".
// Only the errorMessage/message fields are used, never raw bodies, and the result is truncated;
// the *arr apps don't echo request headers, so the key cannot appear here.
func errorDetail(body io.Reader) string {
	var v any
	if json.NewDecoder(io.LimitReader(body, 64*1024)).Decode(&v) != nil {
		return ""
	}
	var msgs []string
	if m, ok := v.(map[string]any); ok {
		if errs, ok := m["errors"].(map[string]any); ok { // ASP.NET problem details
			for _, field := range slices.Sorted(maps.Keys(errs)) {
				if l, ok := errs[field].([]any); ok && len(l) > 0 {
					msgs = append(msgs, fmt.Sprintf("%s: %v", field, l[0]))
				}
			}
			if len(msgs) == 0 {
				msgs = []string{fmt.Sprint(m["title"])}
			}
		}
	}
	if msgs == nil {
		items, ok := v.([]any)
		if !ok {
			items = []any{v}
		}
		var plain []string
		for _, it := range items {
			m, _ := it.(map[string]any)
			if em, _ := m["errorMessage"].(string); em != "" {
				if pn, _ := m["propertyName"].(string); pn != "" {
					em = pn + ": " + em
				}
				msgs = append(msgs, em)
			}
			if msg, ok := m["message"]; ok && msg != nil && msg != "" {
				plain = append(plain, fmt.Sprint(msg))
			}
		}
		if msgs == nil {
			msgs = plain
		}
	}
	if msgs == nil {
		return ""
	}
	s := []rune(strings.Join(msgs, "; "))
	return ": " + string(s[:min(len(s), 300)])
}

func reason(err error) string {
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return "timed out"
	}
	if oe, ok := errors.AsType[*net.OpError](err); ok {
		return oe.Err.Error()
	}
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err.Error()
	}
	return err.Error()
}

// raw sends one API call and returns the (size-capped) body.
func (c *Client) raw(method, path string, data any) ([]byte, error) {
	var body io.Reader
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, errorf("%s %s: %v", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, body)
	if err != nil {
		return nil, errorf("%s %s: invalid request", method, path)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "arr-trickle/"+version.Version)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("%s %s: %s", method, path, reason(err)), NoConn: true}
	}
	defer resp.Body.Close()
	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized:
		return nil, httpErrorf(code, "%s %s: HTTP 401 (check the API key)", method, path)
	case code >= 400 && code < 500:
		return nil, httpErrorf(code, "%s %s: HTTP %d%s", method, path, code, errorDetail(resp.Body))
	case code >= 300:
		return nil, httpErrorf(code, "%s %s: HTTP %d", method, path, code)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, errorf("%s %s: %s", method, path, reason(err))
	}
	if int64(len(b)) > MaxResponseBytes {
		return nil, errorf("%s %s: response larger than %d bytes", method, path, MaxResponseBytes)
	}
	if len(b) > 0 && !json.Valid(b) {
		return nil, errorf("%s %s: response is not JSON (wrong URL or base path?)", method, path)
	}
	return b, nil
}

// Request sends one API call and decodes the JSON answer (nil for an empty body).
func (c *Client) Request(method, path string, data any) (any, error) {
	b, err := c.raw(method, path, data)
	if err != nil || len(b) == 0 {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(b, &v)
}

// call decodes the answer into T; a valid JSON answer of another shape is an error too.
func call[T any](c *Client, method, path string, data any) (T, error) {
	var v T
	b, err := c.raw(method, path, data)
	if err == nil && json.Unmarshal(b, &v) != nil {
		route, _, _ := strings.Cut(path, "?")
		err = errorf("%s %s: unexpected response shape", method, route)
	}
	return v, err
}

// Status is /system/status.
func (c *Client) Status() (map[string]any, error) {
	return call[map[string]any](c, http.MethodGet, "/system/status", nil)
}

// Health is /health.
func (c *Client) Health() ([]map[string]any, error) {
	return call[[]map[string]any](c, http.MethodGet, "/health", nil)
}

// QueueSize is the number of items in the download queue.
func (c *Client) QueueSize() (int, error) {
	q, err := call[struct{ TotalRecords *int }](c, http.MethodGet, "/queue?page=1&pageSize=1", nil)
	if err == nil && q.TotalRecords == nil {
		err = errorf("GET /queue: unexpected response shape")
	}
	if err != nil {
		return 0, err
	}
	return *q.TotalRecords, nil
}

// UpgradeProfiles is the ids of quality profiles that allow upgrades; cutoff searches elsewhere are wasted.
func (c *Client) UpgradeProfiles() (map[int]bool, error) {
	profiles, err := call[[]struct {
		ID             int
		UpgradeAllowed bool
	}](c, http.MethodGet, "/qualityprofile", nil)
	out := map[int]bool{}
	for _, p := range profiles {
		if p.UpgradeAllowed {
			out[p.ID] = true
		}
	}
	return out, err
}

// Wanted is one page (from 1) of /wanted/{missing,cutoff}, least recently searched first.
func (c *Client) Wanted(kind string, pageNo, pageSize int) (WantedPage, error) {
	q := url.Values{
		"page": {strconv.Itoa(pageNo)}, "pageSize": {strconv.Itoa(pageSize)}, "monitored": {"true"},
		"sortKey": {c.Spec.SortKey}, "sortDirection": {"ascending"},
	}
	if c.Spec.Include != "" {
		q.Set(c.Spec.Include, "true")
	}
	page, err := call[struct {
		Records      []apps.Record
		SortKey      string
		TotalRecords *int
	}](c, http.MethodGet, "/wanted/"+kind+"?"+q.Encode(), nil)
	if err == nil && page.Records == nil {
		err = errorf("GET /wanted/%s: unexpected response shape", kind)
	}
	if err != nil {
		return WantedPage{}, err
	}
	total := len(page.Records)
	if page.TotalRecords != nil {
		total = *page.TotalRecords
	}
	return WantedPage{page.Records, page.SortKey, total}, nil
}

// CommandStatus is how a command sent earlier stands: the app's status ("queued", "started",
// "completed", "failed", ...), "unsuccessful" for one that completed with a failed result, or
// "gone" when the app no longer knows the id (it was restarted, or cleaned up old commands).
func (c *Client) CommandStatus(id int) (string, error) {
	cmd, err := call[struct{ Status, Result string }](c, http.MethodGet, "/command/"+strconv.Itoa(id), nil)
	if e, ok := errors.AsType[*Error](err); ok && e.Status == http.StatusNotFound {
		return "gone", nil
	}
	if err != nil {
		return "", err
	}
	if cmd.Status == "completed" && cmd.Result == "unsuccessful" {
		return "unsuccessful", nil
	}
	return cmd.Status, nil
}

// Command sends a /command (e.g. a search), retrying a 5xx after each of CommandRetryDelays.
func (c *Client) Command(body map[string]any) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		v, err := call[map[string]any](c, http.MethodPost, "/command", body)
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Status < 500 || attempt == len(CommandRetryDelays) {
			return v, err
		}
		slog.Warn("command failed, retrying", "app", c.Spec.Kind, "err", err, "in", CommandRetryDelays[attempt])
		time.Sleep(CommandRetryDelays[attempt])
	}
}
