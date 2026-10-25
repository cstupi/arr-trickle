package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Header starts every file the web UI writes.
const Header = "# arr-trickle settings, written by the web UI.\n" +
	"# Comments are not preserved when saving from the UI; a copy of the previous file is kept as config.toml.bak.\n" +
	"# This file holds no secrets: each instance names where its API key is read from.\n"

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tomlString is a TOML basic string with every special character escaped.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlValue(v any) string {
	switch t := v.(type) {
	case bool:
		return fmt.Sprint(t)
	case int:
		return fmt.Sprint(t)
	case int64:
		return fmt.Sprint(t)
	case string:
		return tomlString(t)
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = tomlValue(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	panic(fmt.Sprintf("cannot write %T to TOML", v)) // a programming error: the schema has no other types
}

func writeKeys(b *strings.Builder, m map[string]any, order []string) {
	for _, k := range order {
		if v, ok := m[k]; ok {
			key := k
			if !bareKey.MatchString(k) {
				key = tomlString(k)
			}
			fmt.Fprintf(b, "%s = %s\n", key, tomlValue(v))
		}
	}
}

var (
	topOrder      = []string{"interval_minutes", "state_dir", "dry_run", "log_level", "allow_insecure_http"}
	webOrder      = []string{"enabled", "listen", "port", "cookie_secure", "session_hours", "allowed_networks", "trusted_proxies"}
	instanceOrder = []string{"name", "url", "api_key_from", "api_key_file", "api_key_env", "missing", "cutoff", "daily_cap", "max_queue", "exclude", "timeout_seconds"}
)

// Dump writes settings as TOML (tomllib-style readers can't write, so this is a small writer for this schema).
func Dump(s Settings) string {
	data := ToData(s)
	var b strings.Builder
	b.WriteString(Header + "\n")
	writeKeys(&b, data, topOrder)
	if web, ok := data["web"].(map[string]any); ok {
		b.WriteString("\n[web]\n")
		writeKeys(&b, web, webOrder)
	}
	for _, i := range s.Instances {
		fmt.Fprintf(&b, "\n[[%s]]\n", i.Kind)
		writeKeys(&b, InstanceData(i), instanceOrder)
	}
	return b.String()
}
