// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-api/gen/ermite"
)

// Backoff is the delay between reconnections: it starts at Min, doubles after
// each failed attempt up to Max and resets once a connection has proved
// healthy. Delays are jittered by ±20 %.
type Backoff struct {
	Min time.Duration
	Max time.Duration
}

// DefaultBackoff is 500 ms growing to 30 s.
var DefaultBackoff = Backoff{Min: 500 * time.Millisecond, Max: 30 * time.Second}

// healthyAfter is how long a stream must stay up to count as healthy when it
// delivered no event, for example on a quiet server.
const healthyAfter = 10 * time.Second

// WatchOptions configures Watch.
type WatchOptions struct {
	// Topics are the subjects to follow, see WatchRequest.topics.
	Topics []string
	// ResumeAfter is the id of the last event already seen, or "".
	ResumeAfter string
	// Backoff overrides DefaultBackoff when Min is set.
	Backoff Backoff
	// Sleep waits for d or until ctx ends; nil sleeps for real. For tests.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnNotice receives connection notices (lost, retrying, back); optional.
	OnNotice func(msg string)
}

// Watch follows EventService.Watch and calls onEvent for each event, in
// order, including Gap events (the caller decides how to tell the user). When
// the connection drops or the server closes the stream it reconnects with
// backoff, resuming after the last event id it saw. It returns nil when ctx
// ends, onEvent's error if it fails, and a described error when the server
// refuses the call for a reason a retry cannot fix (authentication, access,
// invalid topic, unsupported call).
func (c *Client) Watch(ctx context.Context, opts WatchOptions, onEvent func(*ermite.Event) error) error {
	log := zerowrap.FromCtx(ctx)
	bo := opts.Backoff
	if bo.Min <= 0 {
		bo = DefaultBackoff
	}
	bo.Max = max(bo.Max, bo.Min)
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	notice := func(msg string) {
		if opts.OnNotice != nil {
			opts.OnNotice(msg)
		}
	}

	events := c.Events()
	last := opts.ResumeAfter
	delay := bo.Min
	retrying := false
	for {
		started := time.Now()
		received := false
		stream, err := events.Watch(ctx, connect.NewRequest(&ermite.WatchRequest{Topics: opts.Topics, ResumeAfter: last}))
		if err == nil {
			for stream.Receive() {
				if !received && retrying {
					notice("reconnected")
				}
				received = true
				ev := stream.Msg().GetEvent()
				if ev == nil {
					continue
				}
				if err := onEvent(ev); err != nil {
					_ = stream.Close()
					return err
				}
				if ev.GetId() != "" {
					last = ev.GetId()
				}
			}
			err = stream.Err()
			_ = stream.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !retryable(err) {
			return c.Describe(err)
		}

		if received || time.Since(started) >= healthyAfter {
			delay = bo.Min
		}
		wait := jitter(delay)
		reason := "the server closed the stream"
		if err != nil {
			reason = c.Describe(err).Error()
		}
		msg := fmt.Sprintf("connection lost (%s); reconnecting in %s", reason, wait.Round(10*time.Millisecond))
		if last == "" {
			msg += " (no resume point yet: events during the outage will be missed)"
		}
		log.Debug().Str("reason", reason).Dur("wait", wait).Str("resume_after", last).Msg("watch reconnecting")
		notice(msg)
		if err := sleep(ctx, wait); err != nil {
			return nil
		}
		delay = min(delay*2, bo.Max)
		retrying = true
	}
}

// retryable reports whether a reconnection can fix the error. The caller has
// checked that its own context is alive, so a cancellation here comes from the
// network (a proxy resetting the stream).
func retryable(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return true
	}
	// A connection cut in the middle of a stream is reported by Connect as an
	// invalid_argument "incomplete envelope" error that the server never sent.
	// Only an invalid_argument that came over the wire (a bad topic) is final.
	if ce.Code() == connect.CodeInvalidArgument && !connect.IsWireError(err) {
		return true
	}
	switch ce.Code() {
	case connect.CodeUnavailable, connect.CodeUnknown, connect.CodeInternal, connect.CodeDeadlineExceeded,
		connect.CodeAborted, connect.CodeResourceExhausted, connect.CodeCanceled:
		return true
	default:
		return false
	}
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
