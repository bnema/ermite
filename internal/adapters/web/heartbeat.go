// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/binary"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Heartbeat is the activity curve of a home row, drawn on a box of
// heartbeatW by heartbeatH that the page scales to its width.
type Heartbeat struct {
	// Line is the curve, Area the same closed down to the bottom edge.
	Line, Area string
	// DaysCommits and DaysVisitors are raw comma-separated daily counts.
	// End is set on a row-local copy, not on the cached curve.
	DaysCommits, DaysVisitors, End string
	// Strip is the daily unique visitors as a heat band under the curve, one
	// cell a day; nil without visitor data.
	Strip []StripCell
	// RecentVisitors sums the daily unique visitors of the last 30 days.
	RecentVisitors uint64
	// X and Y are where the curve ends: the pulse of today.
	X, Y string
	// GradID names the fill gradient of this row (ids are page-wide).
	GradID string
	// Label tells the curve in words.
	Label string
	// Live is set when the last week had commits: the pulse beats.
	Live bool
}

const (
	heartbeatW = 600.0
	heartbeatH = 64.0
	// heartbeatPad keeps the stroke and the dot inside the box; the right
	// end stops before the edge so that the beating ring fits.
	heartbeatPad  = 6.0
	heartbeatEndX = heartbeatW - 10
)

// heartbeatOf draws the commits per day, oldest first. The days are smoothed
// over about a week so that the curve breathes instead of spiking, then
// scaled by the square root so that one big day does not flatten the rest.
// nil for no data.
// partial says the days count only some of the commits (heartbeatLabel).
func heartbeatOf(id string, days []int, partial bool) *Heartbeat {
	n := len(days)
	if n < 2 {
		return nil
	}
	total, lastWeek := 0, 0
	for i, d := range days {
		total += d
		if i >= n-7 {
			lastWeek += d
		}
	}
	top, bottom := heartbeatPad, heartbeatH-heartbeatPad
	var pts [][2]float64
	if total == 0 {
		// Asleep: a straight line on the base, two points.
		pts = [][2]float64{{0, bottom}, {heartbeatEndX, bottom}}
	} else {
		smooth := gaussian(days, 2.2)
		peak := 0.0
		for _, v := range smooth {
			peak = max(peak, math.Sqrt(v))
		}
		// One point every heartbeatStep days, and today: the smoothing
		// leaves nothing finer to draw, and the row goes out on every live
		// update.
		for i := 0; i < n; i += heartbeatStep {
			pts = append(pts, heartbeatPoint(i, n, smooth[i], peak, top, bottom))
		}
		if (n-1)%heartbeatStep != 0 {
			pts = append(pts, heartbeatPoint(n-1, n, smooth[n-1], peak, top, bottom))
		}
	}
	raw := make([]string, len(days))
	for i, d := range days {
		raw[i] = strconv.Itoa(d)
	}
	line := curve(pts)
	end := pts[len(pts)-1]
	return &Heartbeat{
		DaysCommits: strings.Join(raw, ","),
		Line:        line,
		Area:        line + " L" + num(heartbeatEndX) + " " + num(heartbeatH) + " L0 " + num(heartbeatH) + " Z",
		X:           num(end[0]),
		Y:           num(end[1]),
		GradID:      "hb-" + id,
		Label:       heartbeatLabel(total, n, partial),
		Live:        lastWeek > 0,
	}
}

// heartbeatStep is the number of days between two points of the curve.
const heartbeatStep = 2

func heartbeatPoint(i, n int, v, peak, top, bottom float64) [2]float64 {
	return [2]float64{float64(i) / float64(n-1) * heartbeatEndX, bottom - math.Sqrt(v)/peak*(bottom-top)}
}

func heartbeatLabel(total, days int, partial bool) string {
	count := plural(total, "commit", "commits")
	if partial {
		count = "at least " + count
	}
	return "Activity: " + count + " in the last " + itoa(days) + " days"
}

// pulseCache keeps the curves already drawn: the figures of a row stay the
// same for minutes (the stats cache of Browse) while the home page and its
// live updates render the row on every request. Keyed by the repository and
// its days, so a new count draws a new curve; bounded, cleared when full.
type pulseCache struct {
	mu sync.Mutex
	m  map[pulseKey]*Heartbeat
}

type pulseKey struct {
	id       string
	days     string // the counts, packed
	visitors string // visitor counts, packed; empty when unavailable
	partial  bool
}

// maxPulses bounds pulseCache; a few per repository at most are live.
const maxPulses = 4096

// get is heartbeatOf, from the cache when it was drawn already. The
// Heartbeat returned is shared: callers must not change it.
func (c *pulseCache) get(id string, days []int, partial bool, visitors ...[]uint64) *Heartbeat {
	if len(days) < 2 {
		return nil
	}
	packed := make([]byte, 0, len(days)*2)
	for _, d := range days {
		packed = binary.AppendUvarint(packed, uint64(max(d, 0)))
	}
	var daily []uint64
	if len(visitors) > 0 && len(visitors[0]) == len(days) {
		daily = visitors[0]
	}
	vp := make([]byte, 0, len(daily)*2)
	for _, d := range daily {
		vp = binary.AppendUvarint(vp, d)
	}
	key := pulseKey{id: id, days: string(packed), visitors: string(vp), partial: partial}
	c.mu.Lock()
	h, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return h
	}
	h = heartbeatOf(id, days, partial)
	if daily != nil {
		var total uint64
		for _, d := range daily {
			total += d
		}
		raw := make([]string, len(daily))
		for i, d := range daily {
			raw[i] = strconv.FormatUint(d, 10)
		}
		h.DaysVisitors = strings.Join(raw, ",")
		h.Strip = stripOf(daily)
		for _, d := range daily[max(len(daily)-recentDays, 0):] {
			h.RecentVisitors += d
		}
		h.Label = strings.Replace(h.Label, " in the last ", ", "+strconv.FormatUint(total, 10)+" visitors in the last ", 1)
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= maxPulses {
		c.m = make(map[pulseKey]*Heartbeat)
	}
	c.m[key] = h
	c.mu.Unlock()
	return h
}

// recentDays is the window of the recent figures of a row.
const recentDays = 30

// StripCell is one day of the visitor strip, in the units of the curve: X and
// W place it under its day, Level is its fill opacity ("" for a day without
// visitors).
type StripCell struct {
	X, W, Level string
}

// stripOf draws the daily visitors as cells centred on the days of the curve.
// The scale is linear and the row's own, its busiest day at full strength, so
// that a spike stands out; the figures of the row tell the size.
func stripOf(daily []uint64) []StripCell {
	n := len(daily)
	if n < 2 {
		return nil
	}
	peak := slices.Max(daily)
	w := heartbeatEndX / float64(n-1)
	cells := make([]StripCell, n)
	for i, d := range daily {
		// rounded ends, shared by neighbours: no seam between two cells
		x0 := math.Round(max(float64(i)*w-w/2, 0)*10) / 10
		x1 := math.Round(min(float64(i)*w+w/2, heartbeatW)*10) / 10
		c := StripCell{X: num(x0), W: num(x1 - x0)}
		if d > 0 {
			c.Level = strconv.FormatFloat(0.15+0.85*float64(d)/float64(peak), 'f', 2, 64)
		}
		cells[i] = c
	}
	return cells
}

// gaussian blurs the values with a gaussian of the given width, the edges
// clamped: a quiet start or end does not drag the curve to zero.
func gaussian(days []int, sigma float64) []float64 {
	r := int(math.Ceil(3 * sigma))
	k := make([]float64, 2*r+1)
	for i := range k {
		d := float64(i - r)
		k[i] = math.Exp(-d * d / (2 * sigma * sigma))
	}
	out := make([]float64, len(days))
	for i := range days {
		sum, w := 0.0, 0.0
		for j, kj := range k {
			at := min(max(i+j-r, 0), len(days)-1)
			sum += kj * float64(days[at])
			w += kj
		}
		out[i] = sum / w
	}
	return out
}

// curve is a smooth path through the points: a Catmull-Rom spline as cubic
// Béziers, its handles clamped to the box so that it never overshoots below
// the base line.
func curve(p [][2]float64) string {
	var b strings.Builder
	b.WriteString("M" + num(p[0][0]) + " " + num(p[0][1]))
	clampY := func(y float64) float64 { return min(max(y, heartbeatPad/2), heartbeatH-heartbeatPad) }
	for i := 0; i < len(p)-1; i++ {
		p0 := p[max(i-1, 0)]
		p1, p2 := p[i], p[i+1]
		p3 := p[min(i+2, len(p)-1)]
		c1x, c1y := p1[0]+(p2[0]-p0[0])/6, clampY(p1[1]+(p2[1]-p0[1])/6)
		c2x, c2y := p2[0]-(p3[0]-p1[0])/6, clampY(p2[1]-(p3[1]-p1[1])/6)
		b.WriteString(" C" + num(c1x) + " " + num(c1y) + " " + num(c2x) + " " + num(c2y) + " " + num(p2[0]) + " " + num(p2[1]))
	}
	return b.String()
}

// num writes a coordinate with one decimal at most.
func num(f float64) string {
	return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64)
}
