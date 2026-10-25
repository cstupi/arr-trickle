// Package client is a minimal *arr REST client (standard library only).
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/version"
)

// MaxResponseBytes caps a response: wanted pages are small; anything far larger means something is wrong (or hostile).
var MaxResponseBytes int64 = 16 * 1024 * 1024

// Error is a failed request. The message never includes the API key.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errorf(format string, args ...any) *Error { return &Error{fmt.Sprintf(format, args...)} }

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
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
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
func errorDetail(code int, body io.Reader) string {
	if code < 400 || code >= 500 {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(body, 64*1024))
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	trunc := func(msgs []string) string {
		s := strings.Join(msgs, "; ")
		if r := []rune(s); len(r) > 300 {
			s = string(r[:300])
		}
		return ": " + s
	}
	if m, ok := v.(map[string]any); ok {
		if errs, ok := m["errors"].(map[string]any); ok { // ASP.NET problem details
			var msgs []string
			for field, list := range errs {
				if l, ok := list.([]any); ok && len(l) > 0 {
					msgs = append(msgs, fmt.Sprintf("%s: %v", field, l[0]))
				}
			}
			if len(msgs) == 0 {
				msgs = []string{fmt.Sprint(m["title"])}
			}
			return trunc(msgs)
		}
	}
	items, ok := v.([]any)
	if !ok {
		items = []any{v}
	}
	var msgs, plain []string
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if em, ok := m["errorMessage"].(string); ok && em != "" {
			if pn, ok := m["propertyName"].(string); ok && pn != "" {
				msgs = append(msgs, pn+": "+em)
			} else {
				msgs = append(msgs, em)
			}
		}
		if msg, ok := m["message"]; ok && msg != nil && msg != "" {
			plain = append(plain, fmt.Sprint(msg))
		}
	}
	if len(msgs) == 0 {
		msgs = plain
	}
	if len(msgs) == 0 {
		return ""
	}
	return trunc(msgs)
}

func reason(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return oe.Err.Error()
	}
	return err.Error()
}

// Request sends one API call and decodes the JSON answer.
func (c *Client) Request(method, path string, data any) (any, error) {
	var body io.Reader
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, errorf("%s %s: %v", method, path, err)
		}
		body = bytes.NewReader(raw)
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
		return nil, errorf("%s %s: %s", method, path, reason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		hint := errorDetail(resp.StatusCode, resp.Body)
		if resp.StatusCode == 401 {
			hint = " (check the API key)"
		}
		return nil, errorf("%s %s: HTTP %d%s", method, path, resp.StatusCode, hint)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, errorf("%s %s: %s", method, path, reason(err))
	}
	if int64(len(raw)) > MaxResponseBytes {
		return nil, errorf("%s %s: response larger than %d bytes", method, path, MaxResponseBytes)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, errorf("%s %s: response is not JSON (wrong URL or base path?)", method, path)
	}
	return v, nil
}

func (c *Client) get(path string) (any, error) { return c.Request(http.MethodGet, path, nil) }

func shape(path string) error { return errorf("GET %s: unexpected response shape", path) }

// Status is /system/status.
func (c *Client) Status() (map[string]any, error) {
	v, err := c.get("/system/status")
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, shape("/system/status")
	}
	return m, nil
}

// Health is /health.
func (c *Client) Health() ([]map[string]any, error) {
	v, err := c.get("/health")
	if err != nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok {
		return nil, shape("/health")
	}
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// QueueSize is the number of items in the download queue.
func (c *Client) QueueSize() (int, error) {
	v, err := c.get("/queue?page=1&pageSize=1")
	if err != nil {
		return 0, err
	}
	m, _ := v.(map[string]any)
	n, ok := apps.Int(m, "totalRecords")
	if !ok {
		return 0, shape("/queue")
	}
	return n, nil
}

// UpgradeProfiles is the ids of quality profiles that allow upgrades; cutoff searches elsewhere are wasted.
func (c *Client) UpgradeProfiles() (map[int]bool, error) {
	v, err := c.get("/qualityprofile")
	if err != nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok {
		return nil, shape("/qualityprofile")
	}
	out := map[int]bool{}
	for _, it := range list {
		m, _ := it.(map[string]any)
		if up, _ := m["upgradeAllowed"].(bool); up {
			if id, ok := apps.Int(m, "id"); ok {
				out[id] = true
			}
		}
	}
	return out, nil
}

// Wanted is one page of /wanted/{missing,cutoff}, least recently searched first.
func (c *Client) Wanted(kind string, pageSize int) (WantedPage, error) {
	q := url.Values{
		"page": {"1"}, "pageSize": {strconv.Itoa(pageSize)}, "monitored": {"true"},
		"sortKey": {c.Spec.SortKey}, "sortDirection": {"ascending"},
	}
	if c.Spec.Include != "" {
		q.Set(c.Spec.Include, "true")
	}
	path := "/wanted/" + kind + "?" + q.Encode()
	v, err := c.get(path)
	if err != nil {
		return WantedPage{}, err
	}
	m, ok := v.(map[string]any)
	list, ok2 := m["records"].([]any)
	if !ok || !ok2 {
		return WantedPage{}, shape("/wanted/" + kind)
	}
	page := WantedPage{Records: make([]apps.Record, 0, len(list))}
	for _, it := range list {
		if r, ok := it.(map[string]any); ok {
			page.Records = append(page.Records, r)
		}
	}
	page.SortKey, _ = m["sortKey"].(string)
	total, ok := apps.Int(m, "totalRecords")
	if !ok {
		total = len(page.Records)
	}
	page.Total = total
	return page, nil
}

// Command sends a /command (e.g. a search).
func (c *Client) Command(body map[string]any) (map[string]any, error) {
	v, err := c.Request(http.MethodPost, "/command", body)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}
