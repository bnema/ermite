// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"regexp"
	"strings"
)

// A Scenario is the traffic of one or more pages: weighted page requests and
// Server-Sent Events streams kept open while the requests run, as the pages
// of real visitors do.
type Scenario struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Requests    []Request `json:"requests"`
	Streams     []Stream  `json:"streams"`
}

// A Request is one page or asset fetched over and over. Weight is its share
// of the requests of the scenario.
type Request struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Weight int    `json:"weight"`
	// Status is the expected status code, 200 when absent.
	Status int `json:"status"`
}

// A Stream is a live updates feed (GET /-/events) held open during the run.
// Count is how many are opened, scaled by the -streams flag.
type Stream struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Count int    `json:"count"`
}

//go:embed scenarios/*.json
var builtin embed.FS

var varRef = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)

// loadScenario reads a built-in scenario by name, or a JSON file by path,
// and replaces the {{name}} references of its paths with vars.
func loadScenario(ref string, vars map[string]string) (*Scenario, error) {
	var data []byte
	var err error
	if strings.HasSuffix(ref, ".json") {
		data, err = os.ReadFile(ref)
	} else {
		data, err = builtin.ReadFile(path.Join("scenarios", ref+".json"))
	}
	if err != nil {
		return nil, fmt.Errorf("scenario %q: %w (built-in: %s)", ref, err, strings.Join(builtinNames(), ", "))
	}
	var sc Scenario
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("scenario %q: %w", ref, err)
	}
	return &sc, sc.resolve(vars)
}

func builtinNames() []string {
	entries, _ := builtin.ReadDir("scenarios")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names
}

// resolve checks the scenario and expands its variables.
func (sc *Scenario) resolve(vars map[string]string) error {
	if len(sc.Requests) == 0 {
		return errors.New("scenario has no requests")
	}
	var missing []string
	expand := func(p string) string {
		return varRef.ReplaceAllStringFunc(p, func(m string) string {
			name := varRef.FindStringSubmatch(m)[1]
			v, ok := vars[name]
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
	}
	for i := range sc.Requests {
		r := &sc.Requests[i]
		if r.Weight <= 0 {
			r.Weight = 1
		}
		if r.Status == 0 {
			r.Status = 200
		}
		if r.Name == "" {
			r.Name = r.Path
		}
		r.Path = expand(r.Path)
	}
	for i := range sc.Streams {
		s := &sc.Streams[i]
		if s.Name == "" {
			s.Name = s.Path
		}
		s.Path = expand(s.Path)
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing -var for %s", strings.Join(missing, ", "))
	}
	return nil
}

// picker draws requests in proportion to their weights.
type picker struct {
	reqs  []Request
	total int
}

func newPicker(reqs []Request) picker {
	p := picker{reqs: reqs}
	for _, r := range reqs {
		p.total += r.Weight
	}
	return p
}

func (p picker) pick(rng *rand.Rand) int {
	n := rng.IntN(p.total)
	for i, r := range p.reqs {
		if n < r.Weight {
			return i
		}
		n -= r.Weight
	}
	return len(p.reqs) - 1
}
