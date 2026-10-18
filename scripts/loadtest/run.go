// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	base     string
	scenario string
	vars     varsFlag
	clients  int
	rate     float64
	duration time.Duration
	warmup   time.Duration
	streams  float64
	token    string
	gzip     bool
	jsonOut  string
}

// sample is one finished request.
type sample struct {
	req     int
	latency time.Duration
	bytes   int64
	err     string // "" when the request got the expected status
}

// job is one request to send; at is when it was due, so that open-loop
// latencies include the time spent waiting for a client (no coordinated
// omission).
type job struct {
	req int
	at  time.Time
}

func run(ctx context.Context, cfg config, sc *Scenario) (*Report, error) {
	base := strings.TrimRight(cfg.base, "/")
	tr := &http.Transport{
		MaxIdleConns:        cfg.clients * 2,
		MaxIdleConnsPerHost: cfg.clients * 2,
		DisableCompression:  true, // the Accept-Encoding header is set by hand
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	defer tr.CloseIdleConnections()

	if err := preflight(ctx, client, cfg, base, sc); err != nil {
		return nil, err
	}

	// Streams are opened first and stay open for the whole run.
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	streamStats := openStreams(sctx, cfg, base, sc)

	if cfg.warmup > 0 {
		fire(ctx, client, cfg, base, sc, cfg.warmup, nil)
	}
	var mu sync.Mutex
	var samples []sample
	collect := func(s sample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	}
	start := time.Now()
	missed := fire(ctx, client, cfg, base, sc, cfg.duration, collect)
	elapsed := time.Since(start)
	scancel()
	streams := streamStats.wait()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("interrupted after %v: %w", elapsed.Round(time.Millisecond), err)
	}

	return newReport(cfg, sc, samples, missed, elapsed, streams), nil
}

// preflight fetches every request once and fails fast on a wrong status, so
// that a typo in a scenario does not turn into a run of errors.
func preflight(ctx context.Context, client *http.Client, cfg config, base string, sc *Scenario) error {
	var errs []string
	for _, r := range sc.Requests {
		s := do(ctx, client, cfg, base, r)
		if s.err != "" {
			errs = append(errs, fmt.Sprintf("%s (%s): %s", r.Name, r.Path, s.err))
		}
	}
	if len(errs) > 0 {
		return errors.New("preflight failed:\n  " + strings.Join(errs, "\n  "))
	}
	return nil
}

// fire sends requests for d, closed-loop or open-loop, and hands each result
// to collect (nil during the warm-up). It returns the open-loop requests that
// found no free client.
//
// The deadline only stops new requests: those in flight or queued when it
// passes still run to the end (bounded by the client timeout) and are
// counted, so that slow responses are not dropped from the figures. Only an
// interruption of ctx cancels them.
func fire(ctx context.Context, client *http.Client, cfg config, base string, sc *Scenario, d time.Duration, collect func(sample)) int64 {
	sched, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	pk := newPicker(sc.Requests)
	var wg sync.WaitGroup

	send := func(j job) {
		s := do(ctx, client, cfg, base, sc.Requests[j.req])
		if ctx.Err() != nil {
			return // interrupted: the run reports no figures
		}
		s.req = j.req
		if !j.at.IsZero() {
			s.latency = time.Since(j.at)
		}
		if collect != nil {
			collect(s)
		}
	}

	if cfg.rate <= 0 {
		for i := range cfg.clients {
			rng := rand.New(rand.NewPCG(uint64(i), uint64(time.Now().UnixNano())))
			wg.Go(func() {
				for sched.Err() == nil {
					send(job{req: pk.pick(rng)})
				}
			})
		}
		wg.Wait()
		return 0
	}

	// The queue absorbs scheduling hiccups of the load generator; a request
	// that finds it full is missed.
	jobs := make(chan job, cfg.clients)
	for range cfg.clients {
		wg.Go(func() {
			for j := range jobs {
				send(j)
			}
		})
	}
	var missed int64
	rng := rand.New(rand.NewPCG(1, uint64(time.Now().UnixNano())))
	interval := time.Duration(float64(time.Second) / cfg.rate)
	next := time.Now()
loop:
	for {
		if wait := time.Until(next); wait > 0 {
			select {
			case <-sched.Done():
				break loop
			case <-time.After(wait):
			}
		}
		if sched.Err() != nil {
			break
		}
		select {
		case jobs <- job{req: pk.pick(rng), at: next}:
		default:
			missed++
		}
		next = next.Add(interval)
	}
	close(jobs)
	wg.Wait()
	if collect == nil {
		return 0
	}
	return missed
}

// do sends one request and reads the whole body.
func do(ctx context.Context, client *http.Client, cfg config, base string, r Request) sample {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+r.Path, nil)
	if err != nil {
		return sample{err: err.Error()}
	}
	header(req, cfg)
	t := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{latency: time.Since(t), err: shortErr(err)}
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close() // fully read: closing frees the connection
	s := sample{latency: time.Since(t), bytes: n}
	switch {
	case err != nil:
		s.err = shortErr(err)
	case resp.StatusCode != r.Status:
		s.err = fmt.Sprintf("status %d, want %d", resp.StatusCode, r.Status)
	}
	return s
}

func header(req *http.Request, cfg config) {
	req.Header.Set("User-Agent", "ermite-loadtest")
	if cfg.gzip {
		req.Header.Set("Accept-Encoding", "gzip, br, zstd")
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// streamConnectTimeout bounds the wait for the headers of a stream.
const streamConnectTimeout = 5 * time.Second

// streamSet follows the open streams of a run.
type streamSet struct {
	wg    sync.WaitGroup
	stats []*streamStat
}

type streamStat struct {
	name    string
	want    int
	opened  atomic.Int64
	refused atomic.Int64 // a status other than 200, or a failed dial
	dropped atomic.Int64 // closed by the server before the end of the run
	events  atomic.Int64
	ttfb    struct {
		sync.Mutex
		all []time.Duration
	}
}

func openStreams(ctx context.Context, cfg config, base string, sc *Scenario) *streamSet {
	set := &streamSet{}
	if cfg.streams <= 0 {
		return set
	}
	// Each stream is one visitor: its own connection and, against a loopback
	// server, its own source address (127.1.x.y), since the server bounds the
	// streams per client address.
	loopback := isLoopback(base)
	n := 0
	for _, s := range sc.Streams {
		st := &streamStat{name: s.Name, want: int(float64(s.Count)*cfg.streams + .5)}
		set.stats = append(set.stats, st)
		for range st.want {
			client := streamClient(loopback, n)
			n++
			set.wg.Go(func() { follow(ctx, client, cfg, base+s.Path, st) })
		}
	}
	// Let them connect before the measured run starts; a stream still not
	// open after streamConnectTimeout has failed (Report.Failed).
	deadline := time.Now().Add(streamConnectTimeout + time.Second)
	for time.Now().Before(deadline) {
		done := true
		for _, st := range set.stats {
			if int(st.opened.Load()+st.refused.Load()) < st.want {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return set
}

func isLoopback(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Is4() && ip.IsLoopback()
}

// streamClient is the client of the n-th stream.
func streamClient(loopback bool, n int) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if loopback {
		src := netip.AddrFrom4([4]byte{127, 1, byte(n / 250), byte(n%250 + 1)})
		d.LocalAddr = &net.TCPAddr{IP: src.AsSlice()}
	}
	// A stream is a single request: no keep-alive, so that a stream the server
	// closes frees its connection at once. A stream whose headers do not come
	// in time counts as refused.
	return &http.Client{Transport: &http.Transport{
		DialContext:           d.DialContext,
		DisableCompression:    true,
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: streamConnectTimeout,
	}}
}

func follow(ctx context.Context, client *http.Client, cfg config, target string, st *streamStat) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		st.refused.Add(1)
		return
	}
	header(req, cfg)
	req.Header.Del("Accept-Encoding")
	req.Header.Set("Accept", "text/event-stream")
	t := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			st.refused.Add(1)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		st.refused.Add(1)
		return
	}
	st.opened.Add(1)
	st.ttfb.Lock()
	st.ttfb.all = append(st.ttfb.all, time.Since(t))
	st.ttfb.Unlock()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event:") {
			st.events.Add(1)
		}
	}
	if ctx.Err() == nil {
		st.dropped.Add(1)
	}
}

func (s *streamSet) wait() []StreamReport {
	s.wg.Wait()
	out := make([]StreamReport, 0, len(s.stats))
	for _, st := range s.stats {
		out = append(out, StreamReport{
			Name: st.name, Wanted: st.want,
			Opened: int(st.opened.Load()), Refused: int(st.refused.Load()),
			Dropped: int(st.dropped.Load()), Events: int(st.events.Load()),
			Connect: summarize(st.ttfb.all),
		})
	}
	return out
}
