// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/adapters/httpserver"
	"bnema.dev/ermite/internal/adapters/web"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// newWeb builds the web pages on the use cases of this process, through the
// ports (ADR 0020), like the API and Git over HTTP.
//
// The owner's session cookie turns a request into the owner's (web.Server
// checks it with sessions); every other request is anonymous. The web passes
// the principal of the request to each use case call itself.
func newWeb(cfg config.Config, f *forge, limiter *clientlimit.Limiter) (http.Handler, error) {
	return web.New(web.Config{
		Repos:         f.Repos,
		Browse:        f.Browse,
		MergeRequests: f.MergeRequests,
		Events:        f.EventLog,
		PublicURL:     cfg.PublicURL,
		SourceURL:     cfg.SourceURL,
		Version:       serverVersion(),

		Sessions:       webSessions{f.Sessions},
		Limiter:        limiter,
		TrustedProxies: cfg.TrustedProxies,
	})
}

// sessionsNeedHTTPS reports whether browser sign-in cannot work on this public
// URL: the session cookie is Secure (and __Host-), which a browser keeps only
// from https, except on a loopback host (localhost, 127.0.0.0/8, ::1), where
// browsers treat http as secure.
func sessionsNeedHTTPS(publicURL string) bool {
	u, err := url.Parse(publicURL)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err != nil || !ip.Unmap().IsLoopback()
}

// warnSessionsNeedHTTPS tells the operator, at startup, that web sign-in will
// not work on an http public URL. It does not stop the server: the pages,
// the API and Git do not need the cookie.
func warnSessionsNeedHTTPS(ctx context.Context, publicURL string) {
	if !sessionsNeedHTTPS(publicURL) {
		return
	}
	log := zerowrap.FromCtx(ctx)
	log.Warn().Str("public_url", publicURL).Msg(
		"browser sign-in cannot work: " + config.EnvPublicURL + " is http:// on a host that is not loopback, " +
			"and the session cookie is Secure (__Host-), so browsers drop it; serve the site over https")
}

// warnSharedClientKey tells the operator, at startup, that without trusted
// proxies every request is attributed to the address of its TCP peer. Behind a
// reverse proxy that is the proxy: all the clients then share one key, and so
// one cap on the Git backends running at once (maxPerClient) and one budget of
// failed authentications. The keying is unchanged; the fix is to list the
// proxy networks.
func warnSharedClientKey(ctx context.Context, trusted []netip.Prefix, maxPerClient int) {
	if len(trusted) > 0 || maxPerClient <= 0 {
		return
	}
	log := zerowrap.FromCtx(ctx)
	log.Warn().Int("max_git_backends_per_client", maxPerClient).Msg(
		config.EnvTrustedProxies + " is not set: clients are told apart by their TCP peer address only. " +
			"Behind a reverse proxy, all clients share the proxy's address, so one cap of concurrent Git " +
			"requests and one budget of failed authentications; set it to the proxy networks")
}

// webSessions adapts the session use cases to ports.WebSessionService: a valid
// token gives the owner, the principal the web passes to the use cases for
// that request.
type webSessions struct{ *usecase.Sessions }

var _ ports.WebSessionService = webSessions{}

func (s webSessions) Login(ctx context.Context, nonce string) (string, time.Duration, error) {
	token, expiresAt, err := s.Sessions.Login(ctx, nonce)
	if errors.Is(err, domain.ErrUnauthenticated) {
		return "", 0, ports.ErrLinkRejected
	}
	if err != nil {
		return "", 0, err
	}
	// The cookie lasts as long as the session, which the expiry of the token
	// that made the link may cut short. The session token carries its own
	// expiry, so a cookie that outlives it by under a second is harmless.
	// The expiry is truncated to the second, hence the slack on the full TTL.
	lifetime := usecase.SessionTTL
	if d := time.Until(expiresAt); d < usecase.SessionTTL-time.Second {
		lifetime = d.Round(time.Second)
	}
	if lifetime < time.Second {
		return "", 0, ports.ErrLinkRejected
	}
	return token, lifetime, nil
}

func (s webSessions) Verify(ctx context.Context, token string) (domain.Principal, bool, error) {
	p, err := s.Sessions.Verify(ctx, token)
	if errors.Is(err, domain.ErrUnauthenticated) {
		return domain.Anonymous(), false, nil
	}
	if err != nil {
		return domain.Anonymous(), false, err
	}
	return p, true, nil
}

// webRoutes mounts the web on the patterns it serves.
func webRoutes(h http.Handler) []httpserver.Route {
	routes := make([]httpserver.Route, 0, len(web.Patterns))
	for _, p := range web.Patterns {
		routes = append(routes, httpserver.Route{Pattern: p, Handler: h})
	}
	return routes
}
