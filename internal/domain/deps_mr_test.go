package domain

import (
	"slices"
	"strings"
	"testing"
)

func TestBotDraftTitle(t *testing.T) {
	for _, b := range []DepBatch{
		{Updates: []DepUpdate{{DepRef: DepRef{Ecosystem: EcosystemGoMod, Name: "a", Version: "v1.1.0"}}}},
		{},
	} {
		got := BotDraftTitle(b)
		if got != "WIP: "+BotSubject(b) {
			t.Errorf("got %q", got)
		}
		title, err := ParseMRTitle(got)
		if err != nil || !(MergeRequest{Title: title}).IsDraft() {
			t.Errorf("%q not a draft: %v", got, err)
		}
	}
}

func TestDepsRepro(t *testing.T) {
	gm := func(dir, name, v, nn string) DepUpdate {
		return DepUpdate{DepRef: DepRef{Ecosystem: EcosystemGoMod, Directory: dir, Name: name, Version: v}, NewName: nn}
	}
	np := func(dir, name, v string) DepUpdate {
		return DepUpdate{DepRef: DepRef{Ecosystem: EcosystemNPM, Directory: dir, Name: name, Version: v}}
	}
	act := DepUpdate{DepRef: DepRef{Ecosystem: EcosystemActions, Directory: ".github/workflows/ci.yml", Name: "actions/checkout", Version: "v5"}}
	tests := []struct {
		name string
		ups  []DepUpdate
		want []string
	}{
		{"empty", nil, nil},
		{"actions only", []DepUpdate{act}, nil},
		{"gomod root", []DepUpdate{gm("", "b.io/x", "v1.2.0", ""), gm("", "a.io/y", "v0.3.0", "")},
			[]string{"go get a.io/y@v0.3.0", "go get b.io/x@v1.2.0", "go mod tidy"}},
		{"gomod path major", []DepUpdate{gm("tools", "a.io/y", "v2.0.0", "a.io/y/v2")},
			[]string{"cd tools", "# rewrite imports a.io/y -> a.io/y/v2", "go get a.io/y/v2@v2.0.0", "go mod tidy"}},
		{"mixed", []DepUpdate{np("web", "react", "19.0.0"), act, gm("", "a.io/y", "v0.3.0", ""), np("web", "left-pad", "1.3.0")},
			[]string{"go get a.io/y@v0.3.0", "go mod tidy", "cd \"$(git rev-parse --show-toplevel)\"", "cd web", "npm install --ignore-scripts left-pad@1.3.0 react@19.0.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DepsRepro(tt.ups); !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBotMRBodyFor(t *testing.T) {
	ups := []DepUpdate{{DepRef: DepRef{Ecosystem: EcosystemGoMod, Name: "a.io/y", Version: "v0.3.0"}, Current: "v0.2.0"}}
	long := strings.Repeat("x\n", maxBotErrorBytes) + "TAIL"
	tests := []struct {
		name   string
		b      DepBatch
		has    []string
		hasNot []string
	}{
		{"open", DepBatch{State: DepsDrafted}, nil, []string{"update failed", "no longer rewrites"}},
		{"failed no error", DepBatch{State: DepsFailed}, nil, []string{"update failed"}},
		{"failed", DepBatch{State: DepsFailed, Error: "line1\nline2 ```x```", Repro: []string{"go get a.io/y@v0.3.0", "go mod tidy"}},
			[]string{"### Bernard: update failed", "````\nline1\nline2 ```x```\n````", "Reproduce locally:", "```sh\ngo get a.io/y@v0.3.0\ngo mod tidy\n```"}, nil},
		{"failed no repro", DepBatch{State: DepsFailed, Error: "boom"}, []string{"```\nboom\n```"}, []string{"Reproduce locally"}},
		{"long error keeps tail", DepBatch{State: DepsFailed, Error: long}, []string{"TAIL"}, nil},
		{"adopted", DepBatch{State: DepsDrafted, Adopted: true}, []string{"Bernard no longer rewrites it"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BotMRBodyFor(tt.b, ups, nil)
			if !strings.HasPrefix(got, BotMRBody(ups, nil)) {
				t.Errorf("missing base body: %q", got)
			}
			if len(got) > maxBotErrorBytes+1024 {
				t.Errorf("body too long: %d", len(got))
			}
			for _, s := range tt.has {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in %q", s, got)
				}
			}
			for _, s := range tt.hasNot {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in %q", s, got)
				}
			}
		})
	}
}
