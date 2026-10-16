// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// The time zones of schedules do not depend on the host's zoneinfo (the
	// image has none).
	_ "time/tzdata"
)

// Cron is a parsed "cron:" of a schedule trigger: five fields (minute, hour, day
// of month, month, day of week), matched against a wall clock (UTC, or the
// "timezone" of the entry: CronSchedule). The syntax is that of GitHub's schedule: "*", numbers, lists
// "a,b", ranges "a-b", steps "*/n", "a/n" and "a-b/n", the names jan-dec and
// sun-sat in any case; the day of week is 0-6 (Sunday is 0).
//
// As in Vixie cron, when both the day of month and the day of week are
// restricted (neither starts with "*") a day matches if either does; otherwise
// both must.
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
}

type cronField struct {
	min, max int
	names    map[string]int
}

var (
	cronMinuteField = cronField{0, 59, nil}
	cronHourField   = cronField{0, 23, nil}
	cronDomField    = cronField{1, 31, nil}
	cronMonthField  = cronField{1, 12, map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}}
	cronDowField    = cronField{0, 6, map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}}
)

// ParseCron parses the five fields of a cron expression separated by spaces.
func ParseCron(s string) (Cron, error) {
	parts := strings.Fields(s)
	if len(parts) != 5 {
		return Cron{}, fmt.Errorf("%w: cron %q must have 5 fields: minute hour day-of-month month day-of-week", ErrInvalid, s)
	}
	var c Cron
	var err error
	for i, f := range []struct {
		spec cronField
		dst  *uint64
	}{
		{cronMinuteField, &c.minute}, {cronHourField, &c.hour}, {cronDomField, &c.dom}, {cronMonthField, &c.month}, {cronDowField, &c.dow},
	} {
		if *f.dst, err = parseCronField(parts[i], f.spec); err != nil {
			return Cron{}, fmt.Errorf("%w: cron %q: %v", ErrInvalid, s, err)
		}
	}
	c.domStar, c.dowStar = strings.HasPrefix(parts[2], "*"), strings.HasPrefix(parts[4], "*")
	return c, nil
}

func parseCronField(s string, f cronField) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(s, ",") {
		if item == "" {
			return 0, fmt.Errorf("empty item in %q", s)
		}
		rangePart, stepPart, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepPart)
			if err != nil || n < 1 || strings.Trim(stepPart, "0123456789") != "" {
				return 0, fmt.Errorf("invalid step %q", stepPart)
			}
			// A step larger than the field is accepted, as cron does, and keeps
			// only the first value: it is clamped to the size of the field, which
			// has the same meaning and cannot overflow the loop below.
			step = min(n, f.max-f.min+1)
		}
		lo, hi := f.min, f.max
		if rangePart != "*" {
			a, b, isRange := strings.Cut(rangePart, "-")
			var err error
			if lo, err = cronValue(a, f); err != nil {
				return 0, err
			}
			switch {
			case isRange:
				if hi, err = cronValue(b, f); err != nil {
					return 0, err
				}
				if hi < lo {
					return 0, fmt.Errorf("range %q goes backwards", rangePart)
				}
			case hasStep:
				// "a/n" is "a-max/n".
			default:
				hi = lo
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
			if step > hi-v {
				break // the next value is past the range (and v+step could overflow)
			}
		}
	}
	return set, nil
}

func cronValue(s string, f cronField) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || strings.Trim(s, "0123456789") != "" || n < f.min || n > f.max {
		return 0, fmt.Errorf("invalid value %q (%d-%d)", s, f.min, f.max)
	}
	return n, nil
}

// Matches reports whether the minute of t (in UTC) is one the expression fires
// at.
func (c Cron) Matches(t time.Time) bool {
	t = t.UTC()
	if c.minute&(1<<uint(t.Minute())) == 0 || c.hour&(1<<uint(t.Hour())) == 0 || c.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(t.Weekday())) != 0
	if !c.domStar && !c.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// CronSchedule is a cron entry with its time zone (CI-4 D6). GitHub runs the
// entry at the wall clock of the zone, UTC when it has none. In a zone with
// daylight saving time, an entry whose time is skipped by a spring-forward
// transition runs at the next valid time (docs.github.com, "Events that trigger
// workflows", schedule: "a 2:30 AM schedule advances to 3:00 AM"). The docs do
// not say what a fall-back transition does: a wall clock that happens twice
// runs once, at its first occurrence, as cron does.
type CronSchedule struct {
	Cron Cron
	Loc  *time.Location
}

// ParseCronTimezone reads the "timezone" of a schedule entry: an IANA time zone
// name, UTC when empty. "Local" (the server's zone) is not one.
func ParseCronTimezone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if name == "Local" || strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return nil, fmt.Errorf("%w: invalid schedule timezone %q: expected an IANA time zone name such as America/New_York", ErrInvalid, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid schedule timezone %q: expected an IANA time zone name such as America/New_York", ErrInvalid, name)
	}
	return loc, nil
}

// cronGapLimit bounds how far back a transition is looked for: the largest
// clock change of the time zone database (Samoa skipped a whole day in 2011).
const cronGapLimit = 25 * time.Hour

// Due reports whether the entry fires at the minute that starts at t.
func (s CronSchedule) Due(t time.Time) bool {
	loc := s.Loc
	if loc == nil {
		loc = time.UTC
	}
	t = t.Truncate(time.Minute)
	wall := wallMinute(t, loc)
	if s.Cron.Matches(wall) {
		// A wall clock already seen in the last hours (a fall-back
		// transition repeats it) does not fire again.
		return !s.repeated(t, wall, loc)
	}
	// The first minute after a spring-forward gap runs the entries whose wall
	// clock fell in the gap.
	prev := wallMinute(t.Add(-time.Minute), loc)
	for w := prev.Add(time.Minute); w.Before(wall); w = w.Add(time.Minute) {
		if s.Cron.Matches(w) {
			return true
		}
	}
	return false
}

// repeated reports whether the wall clock of t was already shown earlier: an
// instant t-d has it when the offset there is the offset at t plus d, so only
// the larger offsets of the last hours are candidates.
func (s CronSchedule) repeated(t, wall time.Time, loc *time.Location) bool {
	if loc == time.UTC {
		return false
	}
	_, now := t.In(loc).Zone()
	for h := time.Hour; h <= cronGapLimit; h += time.Hour {
		_, off := t.Add(-h).In(loc).Zone()
		if back := time.Duration(off-now) * time.Second; back > 0 && wallMinute(t.Add(-back), loc).Equal(wall) {
			return true
		}
	}
	return false
}

// wallMinute is the wall clock of t in loc, as a UTC time (Matches reads its
// fields).
func wallMinute(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, time.UTC)
}
