// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestRefusalLog() (*refusalLog, *time.Time) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	return &refusalLog{now: func() time.Time { return now }, interval: refusalLogInterval, seen: make(map[string]*refusalEntry)}, &now
}

func TestRefusalLogBurst(t *testing.T) {
	l, _ := newTestRefusalLog()
	for i := range 1000 {
		ok, dropped := l.allow("a")
		if ok != (i == 0) || dropped != 0 {
			t.Fatalf("call %d: got (%v, %d)", i, ok, dropped)
		}
	}
}

func TestRefusalLogInterval(t *testing.T) {
	l, now := newTestRefusalLog()
	for range 1000 {
		l.allow("a")
	}
	*now = now.Add(61 * time.Second)
	if ok, dropped := l.allow("a"); !ok || dropped != 999 {
		t.Fatalf("got (%v, %d), want (true, 999)", ok, dropped)
	}
	if ok, dropped := l.allow("a"); ok || dropped != 0 {
		t.Fatalf("got (%v, %d), want (false, 0)", ok, dropped)
	}
}

func TestRefusalLogIndependentKeys(t *testing.T) {
	l, _ := newTestRefusalLog()
	l.allow("a")
	if ok, dropped := l.allow("b"); !ok || dropped != 0 {
		t.Fatalf("got (%v, %d), want (true, 0)", ok, dropped)
	}
}

func TestRefusalLogSweep(t *testing.T) {
	l, now := newTestRefusalLog()
	for i := range refusalLogMaxKeys {
		l.allow(strconv.Itoa(i))
	}
	*now = now.Add(61 * time.Second)
	if ok, dropped := l.allow("new"); !ok || dropped != 0 {
		t.Fatalf("got (%v, %d), want (true, 0)", ok, dropped)
	}
	if len(l.seen) != 1 {
		t.Fatalf("got %d keys, want 1", len(l.seen))
	}
}

func TestRefusalLogFull(t *testing.T) {
	l, _ := newTestRefusalLog()
	for i := range refusalLogMaxKeys {
		l.allow(strconv.Itoa(i))
	}
	if ok, dropped := l.allow("new"); ok || dropped != 0 {
		t.Fatalf("got (%v, %d), want (false, 0)", ok, dropped)
	}
	if len(l.seen) != refusalLogMaxKeys {
		t.Fatalf("got %d keys", len(l.seen))
	}
}

func TestRefusalLogConcurrent(t *testing.T) {
	l, _ := newTestRefusalLog()
	var trues atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 1000 {
				if ok, _ := l.allow("a"); ok {
					trues.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if trues.Load() != 1 {
		t.Fatalf("got %d trues, want 1", trues.Load())
	}
}

func TestRefusalLogDefersFullSweep(t *testing.T) {
	l, now := newTestRefusalLog()
	for i := range refusalLogMaxKeys {
		l.allow(strconv.Itoa(i))
	}
	l.allow("new") // The first full-table miss schedules the next sweep.
	want := now.Add(refusalLogInterval)
	if !l.nextSweep.Equal(want) {
		t.Fatalf("next sweep = %v, want %v", l.nextSweep, want)
	}
	// Move the entries' expiration forward to detect an unwanted rescan.
	// The remembered deadline remains a safe lower bound.
	for _, entry := range l.seen {
		entry.last = entry.last.Add(time.Second)
	}
	for range 1000 {
		if ok, _ := l.allow("new"); ok {
			t.Fatal("full table admitted a key")
		}
		if !l.nextSweep.Equal(want) {
			t.Fatal("rescanned before nextSweep")
		}
	}
	*now = want.Add(2 * time.Second)
	if ok, dropped := l.allow("new"); !ok || dropped != 0 {
		t.Fatalf("got (%v, %d), want (true, 0)", ok, dropped)
	}
	if len(l.seen) != 1 {
		t.Fatalf("got %d keys, want 1", len(l.seen))
	}
}

func TestRefusalLogEvictionLosesCount(t *testing.T) {
	l, now := newTestRefusalLog()
	l.allow("a")
	l.allow("a")
	for i := range refusalLogMaxKeys - 1 {
		l.allow(strconv.Itoa(i))
	}
	*now = now.Add(61 * time.Second)
	l.allow("new")
	if _, exists := l.seen["a"]; exists {
		t.Fatal("expired entry was not evicted")
	}
	if ok, dropped := l.allow("a"); !ok || dropped != 0 {
		t.Fatalf("got (%v, %d), want (true, 0)", ok, dropped)
	}
}

func TestHandlerRefusalLogsAreIndependent(t *testing.T) {
	h := New(Config{})
	for i := range refusalLogMaxKeys {
		h.clientRefusals.allow(strconv.Itoa(i))
	}
	if ok, _ := h.clientRefusals.allow("extra"); ok {
		t.Fatal("client table is not full")
	}
	if ok, dropped := h.serviceRefusals.allow(serviceUpload); !ok || dropped != 0 {
		t.Fatalf("first busy refusal got (%v, %d), want (true, 0)", ok, dropped)
	}
}
