// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"bnema.dev/ermite/internal/domain"
)

// connectError maps an error of a use case to a Connect error with the code
// the contract documents. Errors it does not know are internal: they are
// logged here and the client only sees a generic message.
func connectError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	if code, ok := codeOf(err); ok {
		ce := connect.NewError(code, err)
		addFieldDetail(ce, err)
		return ce
	}
	log := zerowrap.FromCtx(ctx)
	log.Error().Err(err).Msg("api request failed")
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// codeOf finds the Connect code of a domain error.
func codeOf(err error) (connect.Code, bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled, true
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded, true
	case errors.Is(err, domain.ErrNotFound):
		return connect.CodeNotFound, true
	case errors.Is(err, domain.ErrAlreadyExists):
		return connect.CodeAlreadyExists, true
	case errors.Is(err, domain.ErrUnauthenticated):
		return connect.CodeUnauthenticated, true
	case errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrRejected):
		return connect.CodePermissionDenied, true
	case errors.Is(err, domain.ErrPreconditionFailed):
		return connect.CodeFailedPrecondition, true
	case errors.Is(err, domain.ErrConflict):
		return connect.CodeAborted, true
	case errors.Is(err, domain.ErrResourceExhausted):
		return connect.CodeResourceExhausted, true
	case errors.Is(err, domain.ErrUnavailable):
		return connect.CodeUnavailable, true
	}
	if domain.IsInvalid(err) {
		return connect.CodeInvalidArgument, true
	}
	return 0, false
}

// addFieldDetail attaches the fields a request was rejected for as a
// BadRequest detail, when err carries them.
func addFieldDetail(ce *connect.Error, err error) {
	var fe *domain.InvalidFieldsError
	if !errors.As(err, &fe) {
		return
	}
	br := &errdetails.BadRequest{}
	for _, f := range fe.Fields {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{Field: f.Field, Description: f.Reason})
	}
	if d, derr := connect.NewErrorDetail(br); derr == nil {
		ce.AddDetail(d)
	}
}
