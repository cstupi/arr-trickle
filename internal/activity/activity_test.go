package activity

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cstupi/arr-trickle/internal/runner"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func search(n int, command string, dry bool) runner.Search {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("item %d", i)
	}
	return runner.Search{Wanted: "missing", Command: command, Items: items, Cost: n, DryRun: dry}
}

func TestRecordsAndReadsNewestFirst(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "activity.jsonl"), MaxBytes)
	l.Record("lidarr", search(2, "AlbumSearch", false), tu.Now)
	l.Record("radarr", search(1, "MoviesSearch", true), tu.Now.Add(time.Minute))
	entries := l.Entries("")
	want := Entry{"2026-10-24T00:01:00+00:00", "radarr", "missing", "MoviesSearch", []string{"item 0"}, 1, true}
	if len(entries) != 2 || entries[1].Instance != "lidarr" || !reflect.DeepEqual(entries[0], want) {
		t.Fatal(entries)
	}
	if f := l.Entries("lidarr"); len(f) != 1 || f[0].Instance != "lidarr" {
		t.Fatal(f)
	}
}

func TestRotatesAndStaysBounded(t *testing.T) {
	dir := t.TempDir()
	l := New(filepath.Join(dir, "activity.jsonl"), 1000)
	for i := range 100 {
		l.Record(fmt.Sprintf("inst%d", i), search(1, "AlbumSearch", false), tu.Now)
	}
	for _, name := range []string{"activity.jsonl", "activity.jsonl.1"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Size() > 1000 {
			t.Fatal(name, info, err)
		}
	}
	entries := l.Entries("")
	if entries[0].Instance != "inst99" || len(entries) >= 100 {
		t.Fatal(len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "activity.jsonl.2")); err == nil {
		t.Fatal("only one rotated file is kept")
	}
}

func TestLongItemListsAreTruncated(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "a.jsonl"), MaxBytes)
	items := make([]string, MaxItems+5)
	for i := range items {
		items[i] = strings.Repeat("t", 500)
	}
	l.Record("x", runner.Search{Wanted: "cutoff", Command: "EpisodeSearch", Items: items, Cost: 55}, tu.Now)
	got := l.Entries("")[0].Items
	if len(got) != MaxItems+1 || got[len(got)-1] != "(+5 more)" || len(got[0]) != 200 {
		t.Fatal(len(got), got[len(got)-1], len(got[0]))
	}
}

func TestIgnoresCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	os.WriteFile(path, []byte("not json\n{\"time\": 1}\n{\"x\": 1}\n"), 0o644)
	l := New(path, MaxBytes)
	l.Record("ok", search(1, "AlbumSearch", false), time.Time{})
	if e := l.Entries(""); len(e) != 1 || e[0].Instance != "ok" {
		t.Fatal(e)
	}
}

func TestWriteFailureIsLoggedNotRaised(t *testing.T) {
	logs := tu.CaptureLogs(t)
	l := New(filepath.Join(t.TempDir(), "missing-dir", "a.jsonl"), MaxBytes)
	l.Record("x", search(1, "AlbumSearch", false), tu.Now)
	if !strings.Contains(logs.String(), "could not write the activity log") || len(l.Entries("")) != 0 {
		t.Fatal(logs.String())
	}
}
