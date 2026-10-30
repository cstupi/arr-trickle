// Package planner chooses what to search: pure functions, no I/O.
package planner

import (
	"slices"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
)

// Command is a /command request body.
type Command = map[string]any

// Options narrow what Select may pick.
type Options struct {
	Exclude         []int
	UpgradeProfiles map[int]bool // nil: no profile filter
	PerSeason       bool
	Skip            map[int]bool // record ids not to search (retired)
}

type seasonKey struct{ series, season any }

func key(r apps.Record) seasonKey {
	s, _ := apps.Int(r, "seriesId")
	n, _ := apps.Int(r, "seasonNumber")
	return seasonKey{s, n}
}

// Select picks searchable records in the order given (the app sorts oldest-search first).
//
// Normally up to count records. With PerSeason, records from up to count distinct seasons:
// every missing episode of a chosen season rides along on its SeasonSearch.
func Select(kind string, records []apps.Record, count int, now time.Time, o Options) []apps.Record {
	var picked []apps.Record
	seasons := map[seasonKey]bool{}
	for _, r := range records {
		if !apps.IsReleased(kind, r, now) {
			continue
		}
		if id, _ := apps.Int(r, "id"); o.Skip[id] {
			continue
		}
		if id, ok := apps.OwnerID(kind, r); ok && slices.Contains(o.Exclude, id) {
			continue
		}
		if o.UpgradeProfiles != nil {
			if p, ok := apps.ProfileID(kind, r); !ok || !o.UpgradeProfiles[p] {
				continue
			}
		}
		if o.PerSeason {
			k := key(r)
			if !seasons[k] {
				if len(seasons) >= count {
					continue
				}
				seasons[k] = true
			}
		} else if len(picked) >= count {
			break
		}
		picked = append(picked, r)
	}
	return picked
}

// Full reports whether picked (from Select) already holds count records, or count seasons with PerSeason.
func Full(picked []apps.Record, count int, perSeason bool) bool {
	if !perSeason {
		return len(picked) >= count
	}
	seasons := map[seasonKey]bool{}
	for _, r := range picked {
		seasons[key(r)] = true
	}
	return len(seasons) >= count
}

// IDs is the record ids, in order.
func IDs(records []apps.Record) []int {
	ids := make([]int, 0, len(records))
	for _, r := range records {
		id, _ := apps.Int(r, "id")
		ids = append(ids, id)
	}
	return ids
}

// BuildCommands turns selected records into /command bodies.
func BuildCommands(kind, wanted string, records []apps.Record) []Command {
	if len(records) == 0 {
		return nil
	}
	spec := apps.Specs[kind]
	if kind == "sonarr" && wanted == "missing" {
		var order []seasonKey
		seen := map[seasonKey]bool{}
		for _, r := range records {
			if k := key(r); !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
		cmds := make([]Command, 0, len(order))
		for _, k := range order {
			cmds = append(cmds, Command{"name": "SeasonSearch", "seriesId": k.series, "seasonNumber": k.season})
		}
		return cmds
	}
	name := spec.CutoffCommand
	if wanted == "missing" {
		name = spec.MissingCommand
	}
	return []Command{{"name": name, spec.IDsField: IDs(records)}}
}

// QueryCost approximates the indexer queries a command causes, per indexer.
func QueryCost(cmd Command) int {
	for _, field := range []string{"albumIds", "episodeIds", "movieIds"} {
		if ids, ok := cmd[field].([]int); ok {
			return len(ids)
		}
	}
	return 1
}

// RecordsFor returns the selected records a command covers (for logging).
func RecordsFor(cmd Command, records []apps.Record) []apps.Record {
	if cmd["name"] != "SeasonSearch" {
		return records
	}
	var out []apps.Record
	for _, r := range records {
		k := key(r)
		if k.series == cmd["seriesId"] && k.season == cmd["seasonNumber"] {
			out = append(out, r)
		}
	}
	return out
}
