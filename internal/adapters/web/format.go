// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"strconv"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

// ago renders a short relative date: now, 5 min ago, 3h ago, 12d ago, 5mo
// ago, 2y ago. Minutes are "min", so that they never read as months.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 700*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
}

// absolute is the absolute form shown on hover.
func absolute(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// rfc3339 is the machine form of a time, "" for the zero time.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// grouped writes a count with thousands separated by a thin space: 12 345.
func grouped(n int) string {
	s := itoa(n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var b []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, "\u202f"...)
		}
		b = append(b, s[i])
	}
	return string(b)
}

// viewLabel names a diff layout on the button of the view menu.
func viewLabel(view string) string {
	if view == "split" {
		return "Split"
	}
	return "Unified"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return itoa(n) + " " + many
}

// short is the abbreviated form of a commit id.
func short(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}
