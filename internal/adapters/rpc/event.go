// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// eventService implements ermiteconnect.EventServiceHandler.
type eventService struct{ events ports.EventService }

// Watch streams events until the client goes away or the server stops. The
// stream ends with success in both cases: the contract has no error for
// "server is shutting down". It ends with unauthenticated when the token of
// the caller is revoked or expires, and a caller that has too many streams
// open gets resource_exhausted.
func (s *eventService) Watch(ctx context.Context, req *connect.Request[pb.WatchRequest], stream *connect.ServerStream[pb.WatchResponse]) error {
	params := ports.WatchParams{
		Topics: req.Msg.GetTopics(), ResumeAfter: req.Msg.GetResumeAfter(),
		Client: clientFromContext(ctx),
		// A client's call returns when the response begins: send the headers
		// now, not with the first event, which may be far away. Connect flushes
		// them for a nil message. A failure shows on the first real send.
		Ready: func() { _ = stream.Send(nil) },
	}
	err := s.events.Watch(ctx, caller(ctx), params, func(we ports.WatchEvent) error {
		ev := watchEventToPB(we)
		if ev == nil {
			return nil
		}
		return stream.Send(&pb.WatchResponse{Event: ev})
	})
	return connectError(ctx, err)
}

func releaseEventToPB(we ports.WatchEvent) *pb.Event {
	out := &pb.Event{Id: we.ID}
	switch ev := we.Event.(type) {
	case domain.ReleasePublished:
		r := ev.Release
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_ReleasePublished{ReleasePublished: &pb.ReleasePublished{Repo: r.Repo.String(), Id: r.ID, Tag: r.Tag, Draft: r.Draft, Prerelease: r.Prerelease}}
	case domain.ReleaseUpdated:
		r := ev.Release
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_ReleaseUpdated{ReleaseUpdated: &pb.ReleaseUpdated{Repo: r.Repo.String(), Id: r.ID, Tag: r.Tag, Draft: r.Draft, Prerelease: r.Prerelease}}
	case domain.ReleaseDeleted:
		r := ev.Release
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_ReleaseDeleted{ReleaseDeleted: &pb.ReleaseDeleted{Repo: r.Repo.String(), Id: r.ID, Tag: r.Tag}}
	case domain.ReleaseUnpublished:
		r := ev.Release
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_ReleaseUnpublished{ReleaseUnpublished: &pb.ReleaseUnpublished{Repo: r.Repo.String(), Id: r.ID, Tag: r.Tag, Draft: r.Draft, Prerelease: r.Prerelease}}
	default:
		return nil
	}
	return out
}
