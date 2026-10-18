// SPDX-License-Identifier: AGPL-3.0-or-later

// loadtest replays a traffic scenario against an ermite server and reports
// throughput and latency per request, while holding open the live updates
// streams that the pages of real visitors keep.
//
//	go run ./scripts/loadtest -url http://127.0.0.1:7418 -scenario home
//	go run ./scripts/loadtest -scenario repo -var repo=neferwl -rate 500
//
// Without -rate it runs closed-loop: -c clients send requests back to back,
// which measures the maximum throughput. With -rate it runs open-loop: it
// sends that many requests per second whatever the latency, which measures
// the latency at a target load; a request that finds no free client is
// counted as missed. See scripts/loadtest/README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"
)

func (c config) validate() error {
	switch {
	case c.clients < 1:
		return fmt.Errorf("-c must be at least 1, got %d", c.clients)
	case c.duration <= 0:
		return fmt.Errorf("-d must be positive, got %v", c.duration)
	case c.warmup < 0:
		return fmt.Errorf("-warmup must not be negative, got %v", c.warmup)
	case !(c.streams >= 0) || math.IsInf(c.streams, 0):
		return fmt.Errorf("-streams must be a finite number >= 0, got %v", c.streams)
	case !(c.rate >= 0) || math.IsInf(c.rate, 0):
		return fmt.Errorf("-rate must be a finite number >= 0, got %v", c.rate)
	case c.rate > 1e6:
		return fmt.Errorf("-rate is at most 1000000, got %v", c.rate)
	}
	return nil
}

type varsFlag map[string]string

func (v varsFlag) String() string { return fmt.Sprint(map[string]string(v)) }

func (v varsFlag) Set(s string) error {
	k, val, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want name=value, got %q", s)
	}
	v[k] = val
	return nil
}

func main() {
	cfg := config{vars: varsFlag{}}
	flag.StringVar(&cfg.base, "url", "http://127.0.0.1:7418", "base URL of the server")
	flag.StringVar(&cfg.scenario, "scenario", "home", "built-in scenario name, or path to a .json file")
	flag.Var(cfg.vars, "var", "scenario variable name=value, repeatable")
	flag.IntVar(&cfg.clients, "c", 32, "concurrent clients")
	flag.Float64Var(&cfg.rate, "rate", 0, "requests per second (open-loop); 0 runs closed-loop")
	flag.DurationVar(&cfg.duration, "d", 10*time.Second, "measured duration")
	flag.DurationVar(&cfg.warmup, "warmup", 2*time.Second, "unmeasured warm-up before the run")
	flag.Float64Var(&cfg.streams, "streams", 1, "multiplier of the stream counts of the scenario; 0 opens none")
	flag.StringVar(&cfg.token, "token", "", "bearer token sent with every request (signed-in pages)")
	flag.BoolVar(&cfg.gzip, "gzip", true, "ask for compressed responses, as browsers do")
	flag.StringVar(&cfg.jsonOut, "json", "", "also write the report as JSON to this file")
	flag.Parse()

	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(2)
	}
	sc, err := loadScenario(cfg.scenario, cfg.vars)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rep, err := run(ctx, cfg, sc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
	if err := rep.print(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
	if cfg.jsonOut != "" {
		data, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(cfg.jsonOut, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "loadtest:", err)
			os.Exit(1)
		}
	}
	if rep.Failed() {
		os.Exit(1)
	}
}
