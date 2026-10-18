// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"runtime"
	"slices"
	"text/tabwriter"
	"time"
)

// Report is the result of a run, printed as a table and optionally written as
// JSON to compare runs.
type Report struct {
	Scenario string    `json:"scenario"`
	URL      string    `json:"url"`
	Started  time.Time `json:"started"`
	Mode     string    `json:"mode"` // "closed-loop" or "open-loop"
	Clients  int       `json:"clients"`
	Rate     float64   `json:"target_rps,omitempty"`
	Seconds  float64   `json:"seconds"`
	Client   string    `json:"client"` // the load generator, to tell runs apart
	Total    ReqReport `json:"total"`
	// Missed counts the open-loop requests that found no free client: the
	// server could not keep up with the target rate.
	Missed   int64          `json:"missed"`
	Requests []ReqReport    `json:"requests"`
	Streams  []StreamReport `json:"streams"`
}

type ReqReport struct {
	Name    string         `json:"name"`
	Path    string         `json:"path,omitempty"`
	Count   int            `json:"count"`
	RPS     float64        `json:"rps"`
	Errors  map[string]int `json:"errors,omitempty"`
	Bytes   int64          `json:"avg_bytes"`
	Latency Latency        `json:"latency_ms"`
}

type StreamReport struct {
	Name    string  `json:"name"`
	Wanted  int     `json:"wanted"`
	Opened  int     `json:"opened"`
	Refused int     `json:"refused"`
	Dropped int     `json:"dropped"`
	Events  int     `json:"events"`
	Connect Latency `json:"connect_ms"`
}

type Latency struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

func summarize(d []time.Duration) Latency {
	if len(d) == 0 {
		return Latency{}
	}
	s := slices.Clone(d)
	slices.Sort(s)
	q := func(p float64) float64 {
		return float64(s[int(float64(len(s)-1)*p)]) / float64(time.Millisecond)
	}
	return Latency{P50: q(.5), P90: q(.9), P99: q(.99), Max: q(1)}
}

func newReport(cfg config, sc *Scenario, samples []sample, missed int64, elapsed time.Duration, streams []StreamReport) *Report {
	r := &Report{
		Scenario: sc.Name, URL: cfg.base, Started: time.Now().Add(-elapsed).UTC(),
		Mode: "closed-loop", Clients: cfg.clients, Seconds: elapsed.Seconds(),
		Client:  fmt.Sprintf("%s/%s, %d CPUs", runtime.GOOS, runtime.GOARCH, runtime.NumCPU()),
		Missed:  missed,
		Streams: streams,
	}
	if cfg.rate > 0 {
		r.Mode, r.Rate = "open-loop", cfg.rate
	}
	sum := func(name, path string, ss []sample) ReqReport {
		rr := ReqReport{Name: name, Path: path, Count: len(ss), RPS: float64(len(ss)) / elapsed.Seconds()}
		lat := make([]time.Duration, 0, len(ss))
		var bytes int64
		for _, s := range ss {
			lat = append(lat, s.latency)
			bytes += s.bytes
			if s.err != "" {
				if rr.Errors == nil {
					rr.Errors = map[string]int{}
				}
				rr.Errors[s.err]++
			}
		}
		if len(ss) > 0 {
			rr.Bytes = bytes / int64(len(ss))
		}
		rr.Latency = summarize(lat)
		return rr
	}
	byReq := make([][]sample, len(sc.Requests))
	for _, s := range samples {
		byReq[s.req] = append(byReq[s.req], s)
	}
	for i, q := range sc.Requests {
		r.Requests = append(r.Requests, sum(q.Name, q.Path, byReq[i]))
	}
	r.Total = sum("total", "", samples)
	return r
}

// Failed is true when a request failed, a stream did not open or was
// dropped, or the server missed the target rate.
func (r *Report) Failed() bool {
	if len(r.Total.Errors) > 0 || r.Missed > 0 {
		return true
	}
	for _, s := range r.Streams {
		if s.Opened < s.Wanted || s.Refused > 0 || s.Dropped > 0 {
			return true
		}
	}
	return false
}

// print writes the report as text; it is built in memory (writes to a
// bytes.Buffer cannot fail) and written once.
func (r *Report) print(out io.Writer) error {
	w := &bytes.Buffer{}
	fmt.Fprintf(w, "scenario %s on %s, %s, %d clients", r.Scenario, r.URL, r.Mode, r.Clients)
	if r.Rate > 0 {
		fmt.Fprintf(w, ", target %.0f req/s", r.Rate)
	}
	fmt.Fprintf(w, ", %.1fs (load generator: %s)\n\n", r.Seconds, r.Client)

	tw := &bytes.Buffer{}
	fmt.Fprintln(tw, "request\treq/s\tp50 ms\tp90 ms\tp99 ms\tmax ms\tavg KB\terrors\t")
	row := func(q ReqReport) {
		fmt.Fprintf(tw, "%s\t%.0f\t%.2f\t%.2f\t%.2f\t%.1f\t%.1f\t%d\t\n", q.Name, q.RPS,
			q.Latency.P50, q.Latency.P90, q.Latency.P99, q.Latency.Max, float64(q.Bytes)/1024, errCount(q.Errors))
	}
	for _, q := range r.Requests {
		row(q)
	}
	if len(r.Requests) > 1 {
		row(r.Total)
	}
	table(w, tw)

	for _, q := range append(r.Requests, r.Total) {
		for _, e := range slices.Sorted(maps.Keys(q.Errors)) {
			if q.Name != "total" || len(r.Requests) == 1 {
				fmt.Fprintf(w, "  %s: %d × %s\n", q.Name, q.Errors[e], e)
			}
		}
	}
	if r.Missed > 0 {
		fmt.Fprintf(w, "\nmissed %d requests: the server did not keep up with %.0f req/s\n", r.Missed, r.Rate)
	}
	if len(r.Streams) > 0 {
		w.WriteByte('\n')
		tw.Reset()
		fmt.Fprintln(tw, "stream\topen\trefused\tdropped\tevents\tconnect p99 ms\t")
		for _, s := range r.Streams {
			fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%d\t%d\t%.2f\t\n", s.Name, s.Opened, s.Wanted, s.Refused, s.Dropped, s.Events, s.Connect.P99)
		}
		table(w, tw)
	}
	verdict := "PASS"
	if r.Failed() {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "\n%s\n", verdict)
	_, err := out.Write(w.Bytes())
	return err
}

// table aligns the tab-separated rows of src into dst.
func table(dst, src *bytes.Buffer) {
	tw := tabwriter.NewWriter(dst, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = tw.Write(src.Bytes())
	_ = tw.Flush() // cannot fail: dst is a bytes.Buffer
}

func errCount(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
