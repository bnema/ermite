// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"strconv"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

// cursorText is the initial text of the cursor position in the status bar.
func cursorText(n int) string {
	if n == 0 {
		return "0/0"
	}
	return "1/" + itoa(n)
}

// ago renders a short relative date: now, 5m ago, 3h ago, 12d ago, 5mo ago,
// 2y ago.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
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
