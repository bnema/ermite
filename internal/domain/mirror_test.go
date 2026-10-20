// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMirrorURL(t *testing.T) {
	for _, tt := range []struct {
		s  string
		ok bool
	}{
		{"git@host:acme/repo", true}, {"host:repo", true}, {"ssh://git@host:2222/acme/repo", true}, {"ssh://[::1]/repo", true},
		{"https://host/repo", false}, {"file:///repo", false}, {"git@host:-path", false}, {"ssh://host/-path", false}, {":repo", false}, {"ssh:///repo", false}, {"ssh://git:secret@host/repo", false}, {"user:secret@host:repo", false}, {"host:repo\n", false}, {"ssh://host:0/repo", false},
	} {
		t.Run(tt.s, func(t *testing.T) {
			e := ValidateMirrorURL(tt.s)
			if (e == nil) != tt.ok {
				t.Fatalf("error %v", e)
			}
			if e != nil && !errors.Is(e, ErrInvalid) {
				t.Fatal(e)
			}
		})
	}
	if got := RedactMirrorURL("ssh://git@host:2222/acme/repo"); got != "host:2222:acme/repo" {
		t.Fatal(got)
	}
	if strings.Contains(RedactMirrorURL("ssh://git:secret@host/repo"), "secret") {
		t.Fatal("credential leak")
	}
}
func TestMirrorResolution(t *testing.T) {
	repo, _ := ParseRepoName("demo")
	off := false
	for _, enabled := range []bool{false, true} {
		for _, kind := range []string{"none", "exclude", "override"} {
			t.Run(kind+map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
				g := MirrorGlobalConfig{Enabled: enabled, URLTemplate: "git@host:org/{repo}", Cron: "*/5 * * * *", AllBranches: true}
				var r *MirrorRepoConfig
				want := MirrorSourceNone
				if enabled {
					want = MirrorSourceGlobal
				}
				if kind == "exclude" {
					r = &MirrorRepoConfig{Excluded: true}
					want = MirrorSourceExcluded
				}
				if kind == "override" {
					r = &MirrorRepoConfig{URL: "git@other:demo", Cron: "0 3 * * *", AllBranches: &off, Excluded: true}
					want = MirrorSourceOverride
				}
				got, e := ResolveMirror(g, r, repo)
				if e != nil || got.Source != want || got.Enabled != (want == MirrorSourceGlobal || want == MirrorSourceOverride) {
					t.Fatalf("%+v %v", got, e)
				}
				if want == MirrorSourceOverride && (got.AllBranches || got.CronSpec != "0 3 * * *") {
					t.Fatal(got)
				}
				if want == MirrorSourceGlobal && got.URL != "git@host:org/demo" {
					t.Fatal(got)
				}
			})
		}
	}
	if _, e := ExpandMirrorURL("host:path", repo); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
}
func TestMirrorCronValidation(t *testing.T) {
	for _, tt := range []struct {
		spec  string
		gap   time.Duration
		valid bool
	}{{"*/5 * * * *", 5 * time.Minute, true}, {"*/4 * * * *", 4 * time.Minute, false}, {"0 3 * * *", 24 * time.Hour, true}, {"0 0 29 2 *", 0, true}, {"bad", 0, false}} {
		t.Run(tt.spec, func(t *testing.T) {
			c, e := ParseCron(tt.spec)
			if e == nil && CronMinGap(c) != tt.gap {
				t.Fatal(CronMinGap(c))
			}
			g := MirrorGlobalConfig{Enabled: true, URLTemplate: "git@host:{repo}", Cron: tt.spec}
			if (ValidateMirrorGlobalConfig(g) == nil) != tt.valid {
				t.Fatal("global validation")
			}
			if (ValidateMirrorRepoConfig(MirrorRepoConfig{Cron: tt.spec}) == nil) != tt.valid {
				t.Fatal("repo validation")
			}
		})
	}
	for _, g := range []MirrorGlobalConfig{{}, {URLTemplate: "https://host/{repo}"}, {Enabled: true}, {URLTemplate: "host:path"}} {
		e := ValidateMirrorGlobalConfig(g)
		if g == (MirrorGlobalConfig{}) {
			if e != nil {
				t.Fatal(e)
			}
		} else {
			var fields *InvalidFieldsError
			if !errors.As(e, &fields) {
				t.Fatalf("%v", e)
			}
		}
	}
}
func TestMirrorNextCron(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	c, _ := ParseCron("30 2 * * *")
	start := time.Date(2024, 3, 10, 1, 59, 30, 0, loc)
	got, ok := NextCron(CronSchedule{Cron: c, Loc: loc}, start)
	want := time.Date(2024, 3, 10, 3, 0, 0, 0, loc)
	if !ok || !got.Equal(want) {
		t.Fatal(got, ok)
	}
	c, _ = ParseCron("* * * * *")
	got, ok = NextCron(CronSchedule{Cron: c}, start)
	if !ok || !got.Equal(start.Truncate(time.Minute).Add(time.Minute)) {
		t.Fatal(got)
	}
	if _, ok := NextCron(CronSchedule{}, start); ok {
		t.Fatal("empty cron fired")
	}
}
func TestMirrorTransitions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, class := range []MirrorFailureClass{MirrorClassDiverged, MirrorClassAuthDenied, MirrorClassRemoteNotFound, MirrorClassHostKey, MirrorClassTransient, MirrorClassInternal} {
		t.Run(string(class), func(t *testing.T) {
			p := MirrorStatus{State: MirrorOK}
			fatal := mirrorFatal(class)
			for n := 1; n <= 6; n++ {
				tr := ApplyMirrorOutcome(p, MirrorOutcome{Err: &MirrorError{Class: class, Detail: "detail"}}, now)
				want := MirrorFailing
				if fatal || n >= 5 {
					want = MirrorPaused
				}
				if tr.Status.State != want || tr.Status.ConsecutiveFailures != n || tr.FailedEvent != (n == 1 || !fatal && n == 5) {
					t.Fatalf("n=%d %+v", n, tr)
				}
				if want == MirrorFailing && !tr.Status.NextRetryAt.Equal(now.Add(MirrorBackoff(n))) {
					t.Fatal(tr)
				}
				if want == MirrorPaused && (!tr.Status.NextRetryAt.IsZero() || tr.Status.PausedAt.IsZero()) {
					t.Fatal(tr)
				}
				p = tr.Status
			}
			for _, noop := range []bool{false, true} {
				tr := ApplyMirrorOutcome(p, MirrorOutcome{NoOp: noop}, now)
				if !tr.Recovered || tr.FailedEvent || tr.Status.State != MirrorOK || tr.Status.ConsecutiveFailures != 0 || !tr.Status.LastSuccessAt.Equal(now) || tr.Status.Detail != "" {
					t.Fatal(tr)
				}
				if ApplyMirrorOutcome(tr.Status, MirrorOutcome{}, now).Recovered {
					t.Fatal("repeat recovery")
				}
			}
			for _, state := range []MirrorState{MirrorOK, MirrorDisabled} {
				tr := ApplyMirrorOutcome(MirrorStatus{State: state}, MirrorOutcome{Err: &MirrorError{Class: class}}, now)
				if !tr.FailedEvent {
					t.Fatal(tr)
				}
			}
			if r := MirrorReset(p, now); r.State != MirrorOK || r.Class != "" || !r.PausedAt.IsZero() || r.ConsecutiveFailures != 0 {
				t.Fatal(r)
			}
			if d := MirrorDisable(p, now); d.State != MirrorDisabled || d.Reason != "" {
				t.Fatal(d)
			}
			wantErr := ErrUnavailable
			if fatal {
				wantErr = ErrPreconditionFailed
			}
			if !errors.Is(&MirrorError{Class: class}, wantErr) {
				t.Fatal("unwrap")
			}
		})
	}
	for _, tt := range []struct {
		n    int
		want time.Duration
	}{{1, 5 * time.Minute}, {2, 10 * time.Minute}, {3, 20 * time.Minute}, {7, 320 * time.Minute}, {8, 6 * time.Hour}, {1000000, 6 * time.Hour}} {
		if got := MirrorBackoff(tt.n); got != tt.want {
			t.Fatal(tt, got)
		}
	}
}
func TestMirrorClassify(t *testing.T) {
	for _, tt := range []struct {
		s string
		c MirrorFailureClass
	}{{"non-fast-forward", MirrorClassDiverged}, {"fetch first", MirrorClassDiverged}, {"Permission denied (publickey)", MirrorClassAuthDenied}, {"Repository not found", MirrorClassRemoteNotFound}, {"does not appear to be a git repository", MirrorClassRemoteNotFound}, {"REMOTE HOST IDENTIFICATION HAS CHANGED", MirrorClassHostKey}, {"Host key verification failed", MirrorClassHostKey}, {"timeout", MirrorClassTransient}} {
		if c := ClassifyMirrorOutput(tt.s, errors.New("exit"), false); c != tt.c {
			t.Fatal(tt, c)
		}
	}
	if ClassifyMirrorOutput("", nil, false) != MirrorClassNone || ClassifyMirrorOutput("non-fast-forward", nil, true) != MirrorClassTransient {
		t.Fatal("classification")
	}
}
func TestMirrorRefs(t *testing.T) {
	id, _ := ParseObjectID(strings.Repeat("a", 40))
	refs := []Ref{{Name: "HEAD", ID: id}, {Name: "refs/heads/main", ID: id}, {Name: "refs/heads/other", ID: id}, {Name: "refs/tags/v1", ID: id}, {Name: "refs/pull/1", ID: id}, {Name: "refs/heads/unborn"}}
	for _, all := range []bool{false, true} {
		got := MirrorRefspecs(refs, "main", all)
		n := 2
		if all {
			n = 3
		}
		if len(got) != n {
			t.Fatal(got)
		}
		for i, r := range got {
			if strings.Contains(r.Name+":"+r.Name, "+") || i > 0 && got[i-1].Name >= r.Name {
				t.Fatal(got)
			}
		}
		snap := MirrorSnapshot{URL: "host:repo", Refs: got, AllBranches: all}
		rev := append([]MirrorRef(nil), got...)
		rev[0], rev[len(rev)-1] = rev[len(rev)-1], rev[0]
		if !snap.Equal(snap.URL, rev, all) || snap.Equal("other", rev, all) || snap.Equal(snap.URL, rev, !all) || snap.Equal(snap.URL, rev[1:], all) {
			t.Fatal("snapshot")
		}
		rev[0].ID = ObjectID{}
		if snap.Equal(snap.URL, rev, all) {
			t.Fatal("changed ID")
		}
	}
}
func TestMirrorKey(t *testing.T) {
	for _, tt := range []struct{ url, want string }{{"https://forge.example:443/path", "ermite-mirror@forge.example"}, {"", "ermite-mirror@localhost"}, {":bad", "ermite-mirror@localhost"}} {
		if got := MirrorKeyComment(tt.url); got != tt.want {
			t.Fatal(got)
		}
	}
	pub := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	now := time.Now()
	key := MirrorPublicKeyOf(pub, "forge.example", now)
	parts := strings.Fields(key.AuthorizedKey)
	if len(parts) != 3 || parts[0] != "ssh-ed25519" || parts[2] != "ermite-mirror@forge.example" || !key.CreatedAt.Equal(now) {
		t.Fatal(key)
	}
	wire, e := base64.StdEncoding.DecodeString(parts[1])
	if e != nil || binary.BigEndian.Uint32(wire[:4]) != 11 || string(wire[4:15]) != "ssh-ed25519" || binary.BigEndian.Uint32(wire[15:19]) != 32 || string(wire[19:]) != string(pub) {
		t.Fatal("wire encoding")
	}
	hash := sha256.Sum256(wire)
	if key.Fingerprint != "SHA256:"+base64.RawStdEncoding.EncodeToString(hash[:]) {
		t.Fatal(key)
	}
}

func TestMirrorRecoveryFromFailing(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, state := range []MirrorState{MirrorDisabled, MirrorOK, MirrorFailing, MirrorPaused} {
		for _, noop := range []bool{false, true} {
			tr := ApplyMirrorOutcome(MirrorStatus{State: state, ConsecutiveFailures: 2, Class: MirrorClassTransient, Detail: "old"}, MirrorOutcome{NoOp: noop}, now)
			if tr.Recovered != (state == MirrorFailing || state == MirrorPaused) || tr.FailedEvent || tr.Status.ConsecutiveFailures != 0 || tr.Status.Class != "" || tr.Status.Reason != "" || !tr.Status.NextRetryAt.IsZero() {
				t.Fatal(state, tr)
			}
		}
	}
}
func TestMirrorNextCronFallBack(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	c, _ := ParseCron("30 1 * * *")
	// After the first 01:30, the repeated 01:30 must not fire again.
	start := time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC)
	got, ok := NextCron(CronSchedule{Cron: c, Loc: loc}, start)
	want := time.Date(2024, 11, 4, 1, 30, 0, 0, loc)
	if !ok || !got.Equal(want) {
		t.Fatal(got, ok)
	}
}
