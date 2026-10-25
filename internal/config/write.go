package config

import (
	"fmt"
	"strings"
)

// Header starts every file the web UI writes.
const Header = "# arr-trickle settings, written by the web UI.\n" +
	"# Comments are not preserved when saving from the UI; a copy of the previous file is kept as config.toml.bak.\n" +
	"# This file holds no secrets: each instance names where its API key is read from.\n"

// tomlString is a TOML basic string; quotes, backslashes and control characters are escaped.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String() + `"`
}

func tomlValue(v any) string {
	switch t := v.(type) {
	case bool, int64:
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
			fmt.Fprintf(b, "%s = %s\n", k, tomlValue(v))
		}
	}
}

// Dump writes settings as TOML: a small writer for this schema (the TOML library is only used to read).
func Dump(s Settings) string {
	data := ToData(s)
	var b strings.Builder
	b.WriteString(Header + "\n")
	writeKeys(&b, data, topKeys)
	if web, ok := data["web"].(map[string]any); ok {
		b.WriteString("\n[web]\n")
		writeKeys(&b, web, webKeys)
	}
	for _, i := range s.Instances {
		fmt.Fprintf(&b, "\n[[%s]]\n", i.Kind)
		writeKeys(&b, instanceData(i), instanceKeys)
	}
	return b.String()
}
