package main

import (
	"fmt"
	"hash/fnv"
	"html/template"
	"strings"
	"time"
)

// cloudSVG draws a cloud silhouette that is the same for a given name every time,
// so each app keeps its own shape in the dashboard.
func cloudSVG(name string) template.HTML {
	h := fnv.New64a()
	h.Write([]byte(name))
	seed := h.Sum64() | 1
	next := func(n int) int {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return int(seed % uint64(n))
	}

	var b strings.Builder
	b.WriteString(`<svg viewBox="0 0 64 40" aria-hidden="true" focusable="false"><g fill="currentColor">`)
	b.WriteString(`<rect x="5" y="23" width="54" height="11" rx="5.5"/>`)
	puffs := 3 + next(2)
	span := 40.0
	for i := 0; i < puffs; i++ {
		r := 7 + next(9)
		cx := 12 + span*float64(i)/float64(puffs-1) + float64(next(7)-3)
		cy := 27 - r/2 - next(4)
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%d" r="%d"/>`, cx, cy, r)
	}
	b.WriteString(`</g></svg>`)
	return template.HTML(b.String())
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 14*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
	return t.Local().Format("Jan 2")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
