package state

import (
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
)

// Pending is a search sent but not yet known to have finished: the app's command id and the
// wanted records it covered.
type Pending struct {
	Command int
	Wanted  string // "missing" or "cutoff"
	IDs     []int
}

type retiredEntry struct {
	Items   map[string]map[int]bool // wanted kind -> record ids
	Pending []Pending
}

// Retired remembers, per instance, the wanted records whose search finished cleanly without them
// leaving the wanted list, so passes stop re-searching them; the app's RSS sync still catches new
// releases. Like Budget it survives restarts and is safe to share with the web UI.
type Retired struct {
	path string
	mu   sync.Mutex
	data map[string]*retiredEntry
}

// NewRetired loads the file at path ("" keeps it in memory only).
func NewRetired(path string) *Retired {
	r := &Retired{path: path, data: map[string]*retiredEntry{}}
	if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &r.data) != nil {
		slog.Warn("could not read the retired list; starting it fresh", "path", path)
		r.data = map[string]*retiredEntry{}
	}
	return r
}

// update changes name's entry under the lock and saves the result.
func (r *Retired) update(name string, change func(*retiredEntry)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data[name] == nil {
		r.data[name] = &retiredEntry{Items: map[string]map[int]bool{}}
	}
	change(r.data[name])
	if r.path == "" {
		return nil
	}
	raw, _ := json.Marshal(r.data)
	return AtomicWrite(r.path, raw, 0o644)
}

// read runs f on name's entry, if any, under the lock.
func (r *Retired) read(name string, f func(*retiredEntry)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.data[name]; e != nil {
		f(e)
	}
}

// Set is the retired record ids of one wanted kind (a copy).
func (r *Retired) Set(name, wanted string) (ids map[int]bool) {
	r.read(name, func(e *retiredEntry) { ids = maps.Clone(e.Items[wanted]) })
	return ids
}

// Count is how many records name has retired, all wanted kinds together.
func (r *Retired) Count(name string) (n int) {
	r.read(name, func(e *retiredEntry) {
		for _, ids := range e.Items {
			n += len(ids)
		}
	})
	return n
}

// Pending is a copy of name's searches awaiting an outcome.
func (r *Retired) Pending(name string) (p []Pending) {
	r.read(name, func(e *retiredEntry) { p = slices.Clone(e.Pending) })
	return p
}

// AddPending records a search just sent.
func (r *Retired) AddPending(name string, p Pending) error {
	return r.update(name, func(e *retiredEntry) { e.Pending = append(e.Pending, p) })
}

// Resolve forgets a pending search; if it finished cleanly its records are retired. It returns how
// many records were newly retired.
func (r *Retired) Resolve(name string, command int, clean bool) (added int, err error) {
	err = r.update(name, func(e *retiredEntry) {
		i := slices.IndexFunc(e.Pending, func(p Pending) bool { return p.Command == command })
		if i < 0 {
			return
		}
		p := e.Pending[i]
		e.Pending = slices.Delete(e.Pending, i, i+1)
		if !clean {
			return
		}
		if e.Items[p.Wanted] == nil {
			e.Items[p.Wanted] = map[int]bool{}
		}
		for _, id := range p.IDs {
			if !e.Items[p.Wanted][id] {
				e.Items[p.Wanted][id] = true
				added++
			}
		}
	})
	return added, err
}

// Clear puts every retired record of name back into rotation.
func (r *Retired) Clear(name string) error {
	return r.update(name, func(e *retiredEntry) { *e = retiredEntry{Items: map[string]map[int]bool{}} })
}
