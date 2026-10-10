// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/app"
)

// eventService implements ermiteconnect.EventServiceHandler.
type eventService struct{ events *app.Events }

// Watch streams events until the client goes away or the server stops. The
// stream ends with success in both cases: the contract has no error for
// "server is shutting down". It ends with unauthenticated when the token of
// the caller is revoked or expires, and a caller that has too many streams
// open gets resource_exhausted.
func (s *eventService) Watch(ctx context.Context, req *connect.Request[pb.WatchRequest], stream *connect.ServerStream[pb.WatchResponse]) error {
	params := app.WatchParams{
		Topics: req.Msg.GetTopics(), ResumeAfter: req.Msg.GetResumeAfter(),
		Client: clientFromContext(ctx),
		// A client's call returns when the response begins: send the headers
		// now, not with the first event, which may be far away. Connect flushes
		// them for a nil message. A failure shows on the first real send.
		Ready: func() { _ = stream.Send(nil) },
	}
	err := s.events.Watch(ctx, caller(ctx), params, func(we app.WatchEvent) error {
		ev := watchEventToPB(we)
		if ev == nil {
			return nil
		}
		return stream.Send(&pb.WatchResponse{Event: ev})
	})
	return connectError(ctx, err)
}
