// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02 15:04", s)
	require.NoError(t, err)
	return v
}

func TestCronMatches(t *testing.T) {
	tests := []struct {
		spec  string
		yes   []string
		no    []string
		extra string
	}{
		{"* * * * *", []string{"2026-03-04 05:06", "2026-12-31 23:59"}, nil, ""},
		{"*/15 * * * *", []string{"2026-03-04 05:00", "2026-03-04 05:45"}, []string{"2026-03-04 05:46", "2026-03-04 05:14"}, ""},
		{"30 4 * * *", []string{"2026-03-04 04:30"}, []string{"2026-03-04 04:31", "2026-03-04 05:30"}, ""},
		{"0 0 1 1 *", []string{"2027-01-01 00:00"}, []string{"2027-01-02 00:00", "2027-02-01 00:00"}, ""},
		{"5,10-12 */6 * * *", []string{"2026-03-04 06:05", "2026-03-04 12:11"}, []string{"2026-03-04 06:06", "2026-03-04 07:05"}, ""},
		{"10/20 * * * *", []string{"2026-03-04 00:10", "2026-03-04 00:30", "2026-03-04 00:50"}, []string{"2026-03-04 00:00", "2026-03-04 00:20"}, "a/n runs from a to the maximum"},
		{"0 12 * JAN-mar mon-fri", []string{"2026-02-17 12:00"}, []string{"2026-02-21 12:00", "2026-04-01 12:00"}, "names in any case"},
		{"0 0 * * 0", []string{"2026-03-01 00:00"}, []string{"2026-03-02 00:00"}, "0 is Sunday"},
		// Both day fields restricted: either matches (2026-03-13 is a Friday).
		{"0 0 13 * fri", []string{"2026-03-13 00:00", "2026-03-20 00:00", "2026-04-13 00:00"}, []string{"2026-03-14 00:00"}, "day of month OR day of week"},
		// One of them a star: both must.
		{"0 0 13 * *", []string{"2026-03-13 00:00"}, []string{"2026-03-20 00:00"}, ""},
		{"0 0 * * fri", []string{"2026-03-13 00:00"}, []string{"2026-03-12 00:00"}, ""},
	}
	for _, tt := range tests {
		c, err := domain.ParseCron(tt.spec)
		require.NoError(t, err, tt.spec)
		for _, y := range tt.yes {
			assert.True(t, c.Matches(at(t, y)), "%s at %s %s", tt.spec, y, tt.extra)
		}
		for _, n := range tt.no {
			assert.False(t, c.Matches(at(t, n)), "%s at %s %s", tt.spec, n, tt.extra)
		}
	}
	// Only the UTC minute counts, whatever the zone of the time given.
	c, err := domain.ParseCron("0 10 * * *")
	require.NoError(t, err)
	paris := time.FixedZone("CEST", 2*3600)
	assert.True(t, c.Matches(time.Date(2026, 6, 1, 12, 0, 0, 0, paris)))
	assert.False(t, c.Matches(time.Date(2026, 6, 1, 10, 0, 0, 0, paris)))
}

func TestCronLeapDay(t *testing.T) {
	c, err := domain.ParseCron("0 0 29 2 *")
	require.NoError(t, err)
	assert.True(t, c.Matches(at(t, "2028-02-29 00:00")), "2028 is a leap year")
	assert.False(t, c.Matches(at(t, "2028-02-28 00:00")))
	assert.False(t, c.Matches(at(t, "2028-03-01 00:00")))
	for d := at(t, "2027-02-01 00:00"); d.Month() == time.February; d = d.AddDate(0, 0, 1) {
		assert.False(t, c.Matches(d), "no 29 February in 2027: %s", d)
	}
	any31, err := domain.ParseCron("0 0 31 * *")
	require.NoError(t, err)
	assert.True(t, any31.Matches(at(t, "2026-03-31 00:00")))
	assert.False(t, any31.Matches(at(t, "2026-04-30 00:00")), "no 31st in April")
}

// A step larger than the field keeps its first value only, and cannot overflow.
func TestCronHugeSteps(t *testing.T) {
	for _, spec := range []string{"59/9223372036854775807 * * * *", "*/9223372036854775807 * * * *", "0-30/100 * * * *"} {
		c, err := domain.ParseCron(spec)
		require.NoError(t, err, spec)
		var got []int
		for m := range 60 {
			if c.Matches(at(t, "2026-03-04 05:00").Add(time.Duration(m) * time.Minute)) {
				got = append(got, m)
			}
		}
		switch spec[:2] {
		case "59":
			assert.Equal(t, []int{59}, got, spec)
		default:
			assert.Equal(t, []int{0}, got, spec)
		}
	}
	_, err := domain.ParseCron("*/99999999999999999999 * * * *")
	require.Error(t, err, "beyond an int64")
}

func TestParseCronRefuses(t *testing.T) {
	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 7",
		"*/0 * * * *", "*/x * * * *", "5-3 * * * *", "1,,2 * * * *", "-1 * * * *", "1- * * * *", "foo * * * *", "* * * jun-foo *", "+5 * * * *", "0x5 * * * *",
	} {
		_, err := domain.ParseCron(bad)
		require.Error(t, err, bad)
		assert.ErrorIs(t, err, domain.ErrInvalid, bad)
	}
}

func TestParseCronTimezone(t *testing.T) {
	for _, ok := range []string{"", "UTC", "America/New_York", "Asia/Kolkata", "Europe/Paris"} {
		_, err := domain.ParseCronTimezone(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"Mars/Olympus", "Local", "/etc/localtime", "../zoneinfo/UTC", "new york"} {
		_, err := domain.ParseCronTimezone(bad)
		assert.ErrorIs(t, err, domain.ErrInvalid, bad)
	}
}

// firesOn returns the UTC minutes of the day (from start, for span) at which
// the schedule is due.
func firesOn(t *testing.T, spec, zone string, start time.Time, span time.Duration) []string {
	t.Helper()
	c, err := domain.ParseCron(spec)
	require.NoError(t, err)
	loc, err := domain.ParseCronTimezone(zone)
	require.NoError(t, err)
	s := domain.CronSchedule{Cron: c, Loc: loc}
	var out []string
	for m := start; m.Before(start.Add(span)); m = m.Add(time.Minute) {
		if s.Due(m) {
			out = append(out, m.UTC().Format("2006-01-02 15:04"))
		}
	}
	return out
}

func TestCronScheduleTimezone(t *testing.T) {
	day := func(s string) time.Time { return at(t, s+" 00:00") }
	// Paris in summer is UTC+2.
	assert.Equal(t, []string{"2026-06-01 07:00"}, firesOn(t, "0 9 * * *", "Europe/Paris", day("2026-06-01"), 24*time.Hour))
	// No timezone: UTC.
	assert.Equal(t, []string{"2026-06-01 09:00"}, firesOn(t, "0 9 * * *", "", day("2026-06-01"), 24*time.Hour))
	// The day of week is the zone's: Monday 00:30 in Tokyo is Sunday 15:30 UTC.
	assert.Equal(t, []string{"2026-05-31 15:30"}, firesOn(t, "30 0 * * mon", "Asia/Tokyo", day("2026-05-31"), 24*time.Hour))
}

// New York springs forward on 2026-03-08 (02:00 EST becomes 03:00 EDT) and
// falls back on 2026-11-01 (02:00 EDT becomes 01:00 EST).
func TestCronScheduleDST(t *testing.T) {
	ny := "America/New_York"
	spring, fall := at(t, "2026-03-08 04:00"), at(t, "2026-11-01 03:00") // local midnight, UTC

	// A time the transition skips runs at the next valid time, 03:00 EDT
	// (GitHub's docs: "a 2:30 AM schedule advances to 3:00 AM"), once.
	assert.Equal(t, []string{"2026-03-08 07:00"}, firesOn(t, "30 2 * * *", ny, spring, 24*time.Hour))
	assert.Equal(t, []string{"2026-03-09 06:30"}, firesOn(t, "30 2 * * *", ny, spring.Add(24*time.Hour), 24*time.Hour), "the day after: 02:30 EDT")
	// Times around the gap are not moved, and 03:00 runs once.
	assert.Equal(t, []string{"2026-03-08 06:59"}, firesOn(t, "59 1 * * *", ny, spring, 24*time.Hour))
	assert.Equal(t, []string{"2026-03-08 07:00"}, firesOn(t, "0 3 * * *", ny, spring, 24*time.Hour))
	assert.Equal(t, []string{"2026-03-08 07:00", "2026-03-08 07:15", "2026-03-08 07:30", "2026-03-08 07:45"},
		firesOn(t, "*/15 2-3 * * *", ny, spring, 24*time.Hour), "the skipped 02:00-02:45 entries run once at 03:00, then 03:15-03:45")
	// Hourly: 23 hours that day.
	assert.Len(t, firesOn(t, "0 * * * *", ny, spring, 23*time.Hour), 23)

	// A time that happens twice runs once, at its first occurrence (EDT).
	assert.Equal(t, []string{"2026-11-01 05:30"}, firesOn(t, "30 1 * * *", ny, fall, 25*time.Hour))
	assert.Equal(t, []string{"2026-11-01 07:00"}, firesOn(t, "0 2 * * *", ny, fall, 25*time.Hour), "02:00 EST, once")
	// Hourly: 24 runs over the 25 hours of that day, none at 01:00 EST.
	hourly := firesOn(t, "0 * * * *", ny, fall, 25*time.Hour)
	assert.Len(t, hourly, 24)
	assert.Contains(t, hourly, "2026-11-01 05:00")
	assert.NotContains(t, hourly, "2026-11-01 06:00")
}
