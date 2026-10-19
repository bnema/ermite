// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// Client gives access to the services of one ermite server. Every call carries
// the bearer token and the user agent; unary calls are bounded by the timeout.
// It is safe for concurrent use.
type Client struct {
	cfg  Config
	base string
	http *http.Client
	opts []connect.ClientOption
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
		opts: []connect.ClientOption{connect.WithInterceptors(headers{token: cfg.Token, agent: cfg.UserAgent, timeout: cfg.Timeout})},
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

// Repos returns the RepoService client.
func (c *Client) Repos() ermiteconnect.RepoServiceClient {
	return ermiteconnect.NewRepoServiceClient(c.http, c.base, c.opts...)
}

// Runs returns the RunService client.
func (c *Client) Runs() ermiteconnect.RunServiceClient {
	return ermiteconnect.NewRunServiceClient(c.http, c.base, c.opts...)
}

// Artifacts returns the ArtifactService client.
func (c *Client) Artifacts() ermiteconnect.ArtifactServiceClient {
	return ermiteconnect.NewArtifactServiceClient(c.http, c.base, c.opts...)
}

// Tokens returns the TokenService client.
func (c *Client) Tokens() ermiteconnect.TokenServiceClient {
	return ermiteconnect.NewTokenServiceClient(c.http, c.base, c.opts...)
}

// Sessions returns the SessionService client.
func (c *Client) Sessions() ermiteconnect.SessionServiceClient {
	return ermiteconnect.NewSessionServiceClient(c.http, c.base, c.opts...)
}

// Events returns the EventService client.
func (c *Client) Events() ermiteconnect.EventServiceClient {
	return ermiteconnect.NewEventServiceClient(c.http, c.base, c.opts...)
}

// MergeRequests returns the MergeRequestService client.
func (c *Client) MergeRequests() ermiteconnect.MergeRequestServiceClient {
	return ermiteconnect.NewMergeRequestServiceClient(c.http, c.base, c.opts...)
}

// Issues returns the IssueService client.
func (c *Client) Issues() ermiteconnect.IssueServiceClient {
	return ermiteconnect.NewIssueServiceClient(c.http, c.base, c.opts...)
}

// Secrets returns the SecretService client.
func (c *Client) Secrets() ermiteconnect.SecretServiceClient {
	return ermiteconnect.NewSecretServiceClient(c.http, c.base, c.opts...)
}

// Variables returns the VariableService client.
func (c *Client) Variables() ermiteconnect.VariableServiceClient {
	return ermiteconnect.NewVariableServiceClient(c.http, c.base, c.opts...)
}

// Environments returns the EnvironmentService client.
func (c *Client) Environments() ermiteconnect.EnvironmentServiceClient {
	return ermiteconnect.NewEnvironmentServiceClient(c.http, c.base, c.opts...)
}

// TrustedKeys returns the TrustedKeyService client.
func (c *Client) TrustedKeys() ermiteconnect.TrustedKeyServiceClient {
	return ermiteconnect.NewTrustedKeyServiceClient(c.http, c.base, c.opts...)
}

// Server returns the ServerService client.
func (c *Client) Server() ermiteconnect.ServerServiceClient {
	return ermiteconnect.NewServerServiceClient(c.http, c.base, c.opts...)
}

// headers sets the credentials and the user agent on every call and bounds the
// unary ones.
type headers struct {
	token   string
	agent   string
	timeout time.Duration
}

func (h headers) set(hd http.Header) {
	if h.token != "" {
		hd.Set("Authorization", "Bearer "+h.token)
	}
	if h.agent != "" {
		hd.Set("User-Agent", h.agent)
	}
}

func (h headers) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			h.set(req.Header())
			if h.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, h.timeout)
				defer cancel()
			}
		}
		return next(ctx, req)
	}
}

func (h headers) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		h.set(conn.RequestHeader())
		return conn
	}
}

func (headers) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
