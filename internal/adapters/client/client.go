// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// Client gives access to the services of one ermite server. Every call carries
// the bearer token and the user agent; unary calls are bounded by the timeout.
// It is safe for concurrent use.
type Client struct {
	cfg  Config
	base string
	http *http.Client
	opts []connect.ClientInterceptor
}

// New builds a Client from a validated configuration.
func New(cfg Config) (*Client, error) {
	u, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvURL, err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	cfg.URL = u.String()
	hc := &http.Client{
		Transport: transport(),
		// A redirect would replay a request elsewhere: report it instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Client{
		cfg:  cfg,
		base: u.JoinPath(APIPath).String(),
		http: hc,
		opts: []connect.ClientInterceptor{headers{token: cfg.Token, agent: cfg.UserAgent, timeout: cfg.Timeout}.intercept},
	}, nil
}

// transport bounds connection setup but not the exchange, which may be a
// long-lived stream. HTTP/2 pings detect a connection that died silently.
func transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
	return t
}

// URL is the server address the client talks to.
func (c *Client) URL() string { return c.cfg.URL }

// Mirrors returns the MirrorService client.
func (c *Client) Mirrors() ermiteconnect.MirrorServiceClient {
	return ermiteconnect.NewMirrorServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Repos returns the RepoService client.
func (c *Client) Repos() ermiteconnect.RepoServiceClient {
	return ermiteconnect.NewRepoServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Deps returns the DepsService client.
func (c *Client) Deps() ermiteconnect.DepsServiceClient {
	return ermiteconnect.NewDepsServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Journal returns the JournalService client.
func (c *Client) Journal() ermiteconnect.JournalServiceClient {
	return ermiteconnect.NewJournalServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Runs returns the RunService client.
func (c *Client) Runs() ermiteconnect.RunServiceClient {
	return ermiteconnect.NewRunServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Artifacts returns the ArtifactService client.
func (c *Client) Artifacts() ermiteconnect.ArtifactServiceClient {
	return ermiteconnect.NewArtifactServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Releases returns the ReleaseService client.
func (c *Client) Releases() ermiteconnect.ReleaseServiceClient {
	return ermiteconnect.NewReleaseServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Tokens returns the TokenService client.
func (c *Client) Tokens() ermiteconnect.TokenServiceClient {
	return ermiteconnect.NewTokenServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Sessions returns the SessionService client.
func (c *Client) Sessions() ermiteconnect.SessionServiceClient {
	return ermiteconnect.NewSessionServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Events returns the EventService client.
func (c *Client) Events() ermiteconnect.EventServiceClient {
	return ermiteconnect.NewEventServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// MergeRequests returns the MergeRequestService client.
func (c *Client) MergeRequests() ermiteconnect.MergeRequestServiceClient {
	return ermiteconnect.NewMergeRequestServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Issues returns the IssueService client.
func (c *Client) Issues() ermiteconnect.IssueServiceClient {
	return ermiteconnect.NewIssueServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Secrets returns the SecretService client.
func (c *Client) Secrets() ermiteconnect.SecretServiceClient {
	return ermiteconnect.NewSecretServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Variables returns the VariableService client.
func (c *Client) Variables() ermiteconnect.VariableServiceClient {
	return ermiteconnect.NewVariableServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Environments returns the EnvironmentService client.
func (c *Client) Environments() ermiteconnect.EnvironmentServiceClient {
	return ermiteconnect.NewEnvironmentServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// TrustedKeys returns the TrustedKeyService client.
func (c *Client) TrustedKeys() ermiteconnect.TrustedKeyServiceClient {
	return ermiteconnect.NewTrustedKeyServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// Server returns the ServerService client.
func (c *Client) Server() ermiteconnect.ServerServiceClient {
	return ermiteconnect.NewServerServiceClient(connect.NewClient(connecthttp.NewTransport(c.http, c.base, connecthttp.WithReadMaxBytes(0)), c.opts...))
}

// headers sets the credentials and the user agent on every call and bounds the
// unary ones.
type headers struct {
	token   string
	agent   string
	timeout time.Duration
}

func (h headers) set(hd *connect.Header) {
	if h.token != "" {
		hd.Set("Authorization", "Bearer "+h.token)
	}
	if h.agent != "" {
		hd.Set("User-Agent", h.agent)
	}
}

func (h headers) intercept(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		info, _ := connect.CallInfoForClientContext(ctx)
		h.set(info.RequestHeader())
		var cancel context.CancelFunc
		if spec.StreamType == connect.StreamTypeUnary && h.timeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, h.timeout)
		}
		stream, err := next(ctx, spec)
		if cancel != nil {
			if err != nil {
				cancel()
			} else {
				stream = &timedStream{ClientStream: stream, cancel: cancel}
			}
		}
		return stream, err
	}
}

// timedStream retains the unary deadline until the response has been read.
type timedStream struct {
	connect.ClientStream
	cancel context.CancelFunc
}

func (s *timedStream) Receive(msg any) error { defer s.cancel(); return s.ClientStream.Receive(msg) }
func (s *timedStream) Close() error          { defer s.cancel(); return s.ClientStream.Close() }
