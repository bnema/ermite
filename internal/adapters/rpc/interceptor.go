// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
)

var errInternal = errors.New("internal error")

// interceptor authenticates the caller of every call and logs it.
//
// A request that carries "Authorization: Bearer <secret>" is authenticated
// with it; one without the header is anonymous. A secret that is not valid is
// unauthenticated and counts as a failure of the client address in the
// limiter; a client that used up its failures gets resource_exhausted (429 on
// the Connect protocol) whatever it presents, like on the Git routes.
//
// A context that already carries a principal (domain.ContextWithPrincipal) is
// trusted as it is: that is how the web calls the handlers in process. Such a
// context cannot come from the network.
type interceptor struct {
	auth    Authenticator
	limiter *clientlimit.Limiter
	trusted []netip.Prefix
}

func newInterceptor(cfg Config) *interceptor {
	return &interceptor{auth: cfg.Auth, limiter: cfg.Limiter, trusted: cfg.TrustedProxies}
}

// authenticate returns the context to run the call in.
func (i *interceptor) authenticate(ctx context.Context, procedure, peerAddr string, h http.Header) (context.Context, error) {
	if _, ok := domain.PrincipalFromContext(ctx); ok {
		return ctx, nil
	}
	if procedure == ermiteconnect.ServerServiceGetVersionProcedure {
		// It needs no authentication, so that a client can check compatibility
		// first, even with a token that has expired.
		return domain.ContextWithPrincipal(ctx, domain.Anonymous()), nil
	}
	log := zerowrap.FromCtx(ctx)
	addr := clientlimit.ClientAddr(&http.Request{RemoteAddr: peerAddr, Header: h}, i.trusted)
	if blocked, wait := i.limiter.Blocked(ctx, addr); blocked {
		log.Warn().Str("client", addr).Msg("too many failed authentications, api request blocked")
		err := connect.NewError(connect.CodeResourceExhausted, errors.New("too many failed authentications"))
		err.Meta().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		return ctx, err
	}

	secret, present := bearer(h)
	if !present {
		return domain.ContextWithPrincipal(ctx, domain.Anonymous()), nil
	}
	p, err := i.auth.Authenticate(ctx, secret)
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		i.limiter.Fail(ctx, addr)
		log.Debug().Str("client", addr).Msg("invalid api credentials")
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	case err != nil:
		return ctx, connectError(ctx, err)
	}
	return domain.ContextWithPrincipal(ctx, p), nil
}

// bearer extracts the secret of an "Authorization: Bearer" header. present is
// true when the header exists, even when it carries nothing usable, which
// then fails authentication.
func bearer(h http.Header) (secret string, present bool) {
	auth := h.Get("Authorization")
	if auth == "" {
		return "", false
	}
	scheme, rest, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	return strings.TrimSpace(rest), true
}

// logCall logs a finished call at debug level: procedure, code, caller and
// duration. Never headers or messages: they can carry credentials.
func logCall(ctx context.Context, procedure string, start time.Time, err error) {
	code := "ok"
	if err != nil {
		code = connect.CodeOf(err).String()
	}
	p, _ := domain.PrincipalFromContext(ctx)
	log := zerowrap.FromCtx(ctx)
	log.Debug().
		Str("procedure", procedure).
		Str("code", code).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).
		Dur("duration", time.Since(start)).
		Msg("api call")
}

// WrapUnary implements connect.Interceptor.
func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		ctx, err := i.authenticate(ctx, req.Spec().Procedure, req.Peer().Addr, req.Header())
		var resp connect.AnyResponse
		if err == nil {
			resp, err = next(ctx, req)
		}
		logCall(ctx, req.Spec().Procedure, start, err)
		return resp, err
	}
}

// WrapStreamingClient implements connect.Interceptor; the server has no
// client side.
func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		ctx, err := i.authenticate(ctx, conn.Spec().Procedure, conn.Peer().Addr, conn.RequestHeader())
		if err == nil {
			err = next(ctx, conn)
		}
		logCall(ctx, conn.Spec().Procedure, start, err)
		return err
	}
}
