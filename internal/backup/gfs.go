package backup

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Keep is a GFS retention policy.
type Keep struct {
	Daily, Weekly, Monthly, Yearly int
}

// Item is one existing backup considered for retention.
type Item struct {
	ID   int64
	Time time.Time
}

// Retain decides which backups to keep. It returns the tier labels (e.g. "D,W")
// for kept items; items absent from the map should be deleted.
// The newest backup is always kept.
func Retain(items []Item, k Keep, loc *time.Location) map[int64]string {
	sorted := append([]Item(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.After(sorted[j].Time) })
	tiers := map[int64][]string{}
	type rule struct {
		tag   string
		limit int
		key   func(t time.Time) string
	}
	rules := []rule{
		{"D", k.Daily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{"W", k.Weekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-%02d", y, w) }},
		{"M", k.Monthly, func(t time.Time) string { return t.Format("2006-01") }},
		{"Y", k.Yearly, func(t time.Time) string { return t.Format("2006") }},
	}
	for _, r := range rules {
		if r.limit <= 0 {
			continue
		}
		seen := map[string]bool{}
		for _, it := range sorted {
			if len(seen) >= r.limit {
				break
			}
			key := r.key(it.Time.In(loc))
			if seen[key] {
				continue
			}
			seen[key] = true
			tiers[it.ID] = append(tiers[it.ID], r.tag)
		}
	}
	out := map[int64]string{}
	for id, t := range tiers {
		out[id] = strings.Join(t, ",")
	}
	if len(sorted) > 0 {
		if _, ok := out[sorted[0].ID]; !ok {
			out[sorted[0].ID] = "D"
		}
	}
	return out
}
