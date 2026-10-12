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
	"bnema.dev/ermite/internal/ports"
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
// A principal in the context of a network request, say one put there by a
// server's base context, is ignored and the request is authenticated from its
// headers.
type interceptor struct {
	auth    ports.AuthService
	limiter ports.FailureLimiter
	trusted []netip.Prefix
}

func newInterceptor(cfg Config) *interceptor {
	return &interceptor{auth: cfg.Auth, limiter: cfg.Limiter, trusted: cfg.TrustedProxies}
}

// authenticate returns the context to run the call in.
func (i *interceptor) authenticate(ctx context.Context, procedure, peerAddr string, h http.Header) (context.Context, error) {
	// Whatever the context carried, the caller is anonymous until proven
	// otherwise.
	ctx = domain.ContextWithPrincipal(ctx, domain.Anonymous())
	if procedure == ermiteconnect.ServerServiceGetVersionProcedure || procedure == ermiteconnect.ServerServiceGetSigningKeyProcedure {
		// GetVersion needs no authentication, so that a client can check
		// compatibility first, even with a token that has expired. The signing
		// key is public: whoever verifies a commit needs it.
		return ctx, nil
	}
	log := zerowrap.FromCtx(ctx)
	addr := clientlimit.ClientAddr(&http.Request{RemoteAddr: peerAddr, Header: h}, i.trusted)
	ctx = context.WithValue(ctx, clientKey{}, clientlimit.Key(addr))
	if blocked, wait := i.limiter.Blocked(ctx, addr); blocked {
		log.Warn().Str("client", addr).Msg("too many failed authentications, api request blocked")
		err := connect.NewError(connect.CodeResourceExhausted, errors.New("too many failed authentications"))
		err.Meta().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		return ctx, err
	}

	secret, present := bearer(h)
	if !present {
		return ctx, nil
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

type clientKey struct{}

// clientFromContext returns the normalized address of the client of a call:
// its peer, empty when there is none.
func clientFromContext(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(string)
	return c
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
