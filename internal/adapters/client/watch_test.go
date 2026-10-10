// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

func pushed(id string) *ermite.WatchResponse {
	return &ermite.WatchResponse{Event: &ermite.Event{Id: id, Kind: &ermite.Event_Pushed{Pushed: &ermite.Pushed{Repo: "r"}}}}
}

// sleeps records the waits instead of sleeping.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func TestWatchResumesAfterLastEventAndBacksOff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var requests []*ermite.WatchRequest
	f := &fake{}
	f.watch = func(_ context.Context, req *connect.Request[ermite.WatchRequest], st *connect.ServerStream[ermite.WatchResponse]) error {
		mu.Lock()
		requests = append(requests, req.Msg)
		n := len(requests)
		mu.Unlock()
		switch n {
		case 1: // two events, then the connection ends
			_ = st.Send(pushed("e1"))
			_ = st.Send(pushed("e2"))
			return connect.NewError(connect.CodeUnavailable, errors.New("shutting down"))
		case 2: // fails at once, twice
			return connect.NewError(connect.CodeUnavailable, errors.New("still down"))
		case 3:
			return connect.NewError(connect.CodeUnavailable, errors.New("still down"))
		default: // back: one more event, then the user stops
			_ = st.Send(pushed("e3"))
			<-ctx.Done()
			return nil
		}
	}
	c := start(t, f, client.Config{})

	var got []string
	var notices []string
	sl := &sleeps{}
	err := c.Watch(ctx, client.WatchOptions{
		Topics:   []string{"home"},
		Backoff:  client.Backoff{Min: time.Second, Max: 3 * time.Second},
		Sleep:    sl.sleep,
		OnNotice: func(m string) { notices = append(notices, m) },
	}, func(ev *ermite.Event) error {
		got = append(got, ev.GetId())
		if ev.GetId() == "e3" {
			cancel()
		}
		return nil
	})
	require.NoError(t, err, "stopping with the context is not an error")
	assert.Equal(t, []string{"e1", "e2", "e3"}, got)

	require.Len(t, requests, 4)
	assert.Equal(t, "", requests[0].GetResumeAfter())
	for _, r := range requests[1:3] {
		assert.Equal(t, "e2", r.GetResumeAfter())
	}
	assert.Equal(t, "e2", requests[3].GetResumeAfter())
	assert.Equal(t, []string{"home"}, requests[3].GetTopics())

	// First wait: backoff was reset by the healthy first stream (Min), then it
	// doubles and is capped at Max. Delays are jittered by ±20 %.
	require.Len(t, sl.d, 3)
	assertNear(t, time.Second, sl.d[0])
	assertNear(t, 2*time.Second, sl.d[1])
	assertNear(t, 3*time.Second, sl.d[2])

	require.NotEmpty(t, notices)
	assert.Contains(t, notices[0], "connection lost")
	assert.Contains(t, notices[0], "shutting down")
	assert.Contains(t, notices, "reconnected")
}

func assertNear(t *testing.T, want, got time.Duration) {
	t.Helper()
	assert.InDelta(t, float64(want), float64(got), float64(want)*0.21)
}

func TestWatchWarnsWhenThereIsNoResumePoint(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	f := &fake{}
	f.watch = func(context.Context, *connect.Request[ermite.WatchRequest], *connect.ServerStream[ermite.WatchResponse]) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	c := start(t, f, client.Config{})
	var notices []string
	require.NoError(t, c.Watch(ctx, client.WatchOptions{
		Sleep:    (&sleeps{}).sleep,
		OnNotice: func(m string) { notices = append(notices, m) },
	}, func(*ermite.Event) error { return nil }))
	require.NotEmpty(t, notices)
	assert.Contains(t, notices[0], "no resume point")
}

func TestWatchStopsOnPermanentErrors(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeUnimplemented} {
		t.Run(code.String(), func(t *testing.T) {
			calls := 0
			f := &fake{}
			f.watch = func(context.Context, *connect.Request[ermite.WatchRequest], *connect.ServerStream[ermite.WatchResponse]) error {
				calls++
				return connect.NewError(code, errors.New("nope"))
			}
			c := start(t, f, client.Config{})
			err := c.Watch(t.Context(), client.WatchOptions{Sleep: (&sleeps{}).sleep}, func(*ermite.Event) error { return nil })
			require.Error(t, err)
			assert.Equal(t, 1, calls, "no retry")
			var ce *connect.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, code, ce.Code())
		})
	}
}

func TestWatchReturnsHandlerError(t *testing.T) {
	f := &fake{}
	f.watch = func(ctx context.Context, _ *connect.Request[ermite.WatchRequest], st *connect.ServerStream[ermite.WatchResponse]) error {
		_ = st.Send(pushed("e1"))
		<-ctx.Done()
		return nil
	}
	c := start(t, f, client.Config{})
	boom := errors.New("write failed")
	err := c.Watch(t.Context(), client.WatchOptions{}, func(*ermite.Event) error { return boom })
	assert.ErrorIs(t, err, boom)
}

func TestWatchPassesGapAndIgnoresItAsResumePoint(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var resumes []string
	f := &fake{}
	f.watch = func(_ context.Context, req *connect.Request[ermite.WatchRequest], st *connect.ServerStream[ermite.WatchResponse]) error {
		resumes = append(resumes, req.Msg.GetResumeAfter())
		if len(resumes) == 1 {
			_ = st.Send(pushed("e1"))
			_ = st.Send(&ermite.WatchResponse{Event: &ermite.Event{Kind: &ermite.Event_Gap{Gap: &ermite.Gap{}}}})
			return connect.NewError(connect.CodeUnavailable, errors.New("x"))
		}
		_ = st.Send(pushed("e2"))
		<-ctx.Done()
		return nil
	}
	c := start(t, f, client.Config{})
	var kinds []string
	require.NoError(t, c.Watch(ctx, client.WatchOptions{Sleep: (&sleeps{}).sleep}, func(ev *ermite.Event) error {
		if ev.GetGap() != nil {
			kinds = append(kinds, "gap")
		} else {
			kinds = append(kinds, ev.GetId())
		}
		if ev.GetId() == "e2" {
			cancel()
		}
		return nil
	}))
	assert.Equal(t, []string{"e1", "gap", "e2"}, kinds)
	assert.Equal(t, []string{"", "e1"}, resumes, "a gap has no id and must not erase the resume point")
}

func TestWatchResumesFromGivenID(t *testing.T) {
	var first string
	f := &fake{}
	f.watch = func(_ context.Context, req *connect.Request[ermite.WatchRequest], _ *connect.ServerStream[ermite.WatchResponse]) error {
		first = req.Msg.GetResumeAfter()
		return connect.NewError(connect.CodePermissionDenied, errors.New("stop"))
	}
	c := start(t, f, client.Config{})
	_ = c.Watch(t.Context(), client.WatchOptions{ResumeAfter: "abc"}, func(*ermite.Event) error { return nil })
	assert.Equal(t, "abc", first)
}

// A connection cut mid-stream surfaces as a client-side invalid_argument; it
// must be retried, unlike an invalid_argument the server sent.
func TestWatchRetriesCutConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// Announce a frame and hang up before sending it.
			w.Header().Set("Content-Type", "application/connect+proto")
			_, _ = w.Write([]byte{0, 0, 0, 0, 9})
			return
		}
		cancel()
	}))
	defer srv.Close()
	c, err := client.New(client.Config{URL: srv.URL})
	require.NoError(t, err)
	var notices []string
	require.NoError(t, c.Watch(ctx, client.WatchOptions{
		Sleep:    (&sleeps{}).sleep,
		OnNotice: func(m string) { notices = append(notices, m) },
	}, func(*ermite.Event) error { return nil }))
	assert.GreaterOrEqual(t, calls, 2, "retried")
	require.NotEmpty(t, notices)
	assert.Contains(t, notices[0], "connection lost")
}
