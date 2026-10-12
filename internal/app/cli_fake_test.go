// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// fakeAPI is a Connect server with stub services: each field, when set,
// answers the matching RPC; the others answer unimplemented. It records the
// requests it receives.
type fakeAPI struct {
	ermiteconnect.UnimplementedRepoServiceHandler
	ermiteconnect.UnimplementedTokenServiceHandler
	ermiteconnect.UnimplementedServerServiceHandler
	ermiteconnect.UnimplementedEventServiceHandler
	ermiteconnect.UnimplementedMergeRequestServiceHandler

	mu     sync.Mutex
	calls  []string      // procedure names, in order
	reqs   []any         // request messages, in order
	header []http.Header // request headers, in order

	listRepos   func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error)
	getRepo     func(*ermite.GetRepoRequest) (*ermite.GetRepoResponse, error)
	createRepo  func(*ermite.CreateRepoRequest) (*ermite.CreateRepoResponse, error)
	updateRepo  func(*ermite.UpdateRepoRequest) (*ermite.UpdateRepoResponse, error)
	deleteRepo  func(*ermite.DeleteRepoRequest) (*ermite.DeleteRepoResponse, error)
	createToken func(*ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error)
	listTokens  func(*ermite.ListTokensRequest) (*ermite.ListTokensResponse, error)
	revokeToken func(*ermite.RevokeTokenRequest) (*ermite.RevokeTokenResponse, error)
	getVersion  func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error)
	watch       func(context.Context, *ermite.WatchRequest, *connect.ServerStream[ermite.WatchResponse]) error

	createMR      func(*ermite.CreateMergeRequestRequest) (*ermite.CreateMergeRequestResponse, error)
	listMR        func(*ermite.ListMergeRequestsRequest) (*ermite.ListMergeRequestsResponse, error)
	getMR         func(*ermite.GetMergeRequestRequest) (*ermite.GetMergeRequestResponse, error)
	addComment    func(*ermite.AddCommentRequest) (*ermite.AddCommentResponse, error)
	resolveCmt    func(*ermite.ResolveCommentRequest) (*ermite.ResolveCommentResponse, error)
	closeMR       func(*ermite.CloseMergeRequestRequest) (*ermite.CloseMergeRequestResponse, error)
	mergeMR       func(*ermite.MergeMergeRequestRequest) (*ermite.MergeMergeRequestResponse, error)
	markRead      func(*ermite.MarkReadRequest) (*ermite.MarkReadResponse, error)
	getSigningKey func(*ermite.GetSigningKeyRequest) (*ermite.GetSigningKeyResponse, error)
}

func (f *fakeAPI) record(name string, h http.Header, req any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	f.reqs = append(f.reqs, req)
	f.header = append(f.header, h.Clone())
}

func (f *fakeAPI) snapshot() (calls []string, reqs []any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), append([]any(nil), f.reqs...)
}

func respond[Req, Res any](f *fakeAPI, name string, req *connect.Request[Req], fn func(*Req) (*Res, error)) (*connect.Response[Res], error) {
	f.record(name, req.Header(), req.Msg)
	if fn == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, nil)
	}
	res, err := fn(req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (f *fakeAPI) ListRepos(_ context.Context, r *connect.Request[ermite.ListReposRequest]) (*connect.Response[ermite.ListReposResponse], error) {
	return respond(f, "ListRepos", r, f.listRepos)
}
func (f *fakeAPI) GetRepo(_ context.Context, r *connect.Request[ermite.GetRepoRequest]) (*connect.Response[ermite.GetRepoResponse], error) {
	return respond(f, "GetRepo", r, f.getRepo)
}
func (f *fakeAPI) CreateRepo(_ context.Context, r *connect.Request[ermite.CreateRepoRequest]) (*connect.Response[ermite.CreateRepoResponse], error) {
	return respond(f, "CreateRepo", r, f.createRepo)
}
func (f *fakeAPI) UpdateRepo(_ context.Context, r *connect.Request[ermite.UpdateRepoRequest]) (*connect.Response[ermite.UpdateRepoResponse], error) {
	return respond(f, "UpdateRepo", r, f.updateRepo)
}
func (f *fakeAPI) DeleteRepo(_ context.Context, r *connect.Request[ermite.DeleteRepoRequest]) (*connect.Response[ermite.DeleteRepoResponse], error) {
	return respond(f, "DeleteRepo", r, f.deleteRepo)
}
func (f *fakeAPI) CreateToken(_ context.Context, r *connect.Request[ermite.CreateTokenRequest]) (*connect.Response[ermite.CreateTokenResponse], error) {
	return respond(f, "CreateToken", r, f.createToken)
}
func (f *fakeAPI) ListTokens(_ context.Context, r *connect.Request[ermite.ListTokensRequest]) (*connect.Response[ermite.ListTokensResponse], error) {
	return respond(f, "ListTokens", r, f.listTokens)
}
func (f *fakeAPI) RevokeToken(_ context.Context, r *connect.Request[ermite.RevokeTokenRequest]) (*connect.Response[ermite.RevokeTokenResponse], error) {
	return respond(f, "RevokeToken", r, f.revokeToken)
}
func (f *fakeAPI) GetVersion(_ context.Context, r *connect.Request[ermite.GetVersionRequest]) (*connect.Response[ermite.GetVersionResponse], error) {
	return respond(f, "GetVersion", r, f.getVersion)
}
func (f *fakeAPI) CreateMergeRequest(_ context.Context, r *connect.Request[ermite.CreateMergeRequestRequest]) (*connect.Response[ermite.CreateMergeRequestResponse], error) {
	return respond(f, "CreateMergeRequest", r, f.createMR)
}
func (f *fakeAPI) ListMergeRequests(_ context.Context, r *connect.Request[ermite.ListMergeRequestsRequest]) (*connect.Response[ermite.ListMergeRequestsResponse], error) {
	return respond(f, "ListMergeRequests", r, f.listMR)
}
func (f *fakeAPI) GetMergeRequest(_ context.Context, r *connect.Request[ermite.GetMergeRequestRequest]) (*connect.Response[ermite.GetMergeRequestResponse], error) {
	return respond(f, "GetMergeRequest", r, f.getMR)
}
func (f *fakeAPI) AddComment(_ context.Context, r *connect.Request[ermite.AddCommentRequest]) (*connect.Response[ermite.AddCommentResponse], error) {
	return respond(f, "AddComment", r, f.addComment)
}
func (f *fakeAPI) ResolveComment(_ context.Context, r *connect.Request[ermite.ResolveCommentRequest]) (*connect.Response[ermite.ResolveCommentResponse], error) {
	return respond(f, "ResolveComment", r, f.resolveCmt)
}
func (f *fakeAPI) CloseMergeRequest(_ context.Context, r *connect.Request[ermite.CloseMergeRequestRequest]) (*connect.Response[ermite.CloseMergeRequestResponse], error) {
	return respond(f, "CloseMergeRequest", r, f.closeMR)
}
func (f *fakeAPI) MergeMergeRequest(_ context.Context, r *connect.Request[ermite.MergeMergeRequestRequest]) (*connect.Response[ermite.MergeMergeRequestResponse], error) {
	return respond(f, "MergeMergeRequest", r, f.mergeMR)
}
func (f *fakeAPI) MarkRead(_ context.Context, r *connect.Request[ermite.MarkReadRequest]) (*connect.Response[ermite.MarkReadResponse], error) {
	return respond(f, "MarkRead", r, f.markRead)
}
func (f *fakeAPI) GetSigningKey(_ context.Context, r *connect.Request[ermite.GetSigningKeyRequest]) (*connect.Response[ermite.GetSigningKeyResponse], error) {
	return respond(f, "GetSigningKey", r, f.getSigningKey)
}
func (f *fakeAPI) Watch(ctx context.Context, r *connect.Request[ermite.WatchRequest], st *connect.ServerStream[ermite.WatchResponse]) error {
	f.record("Watch", r.Header(), r.Msg)
	if f.watch == nil {
		return connect.NewError(connect.CodeUnimplemented, nil)
	}
	return f.watch(ctx, r.Msg, st)
}

// serve starts the fake under /-/api and returns its base URL.
func (f *fakeAPI) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mount := func(path string, h http.Handler) { mux.Handle("/-/api"+path, http.StripPrefix("/-/api", h)) }
	p, h := ermiteconnect.NewRepoServiceHandler(f)
	mount(p, h)
	p, h = ermiteconnect.NewTokenServiceHandler(f)
	mount(p, h)
	p, h = ermiteconnect.NewServerServiceHandler(f)
	mount(p, h)
	p, h = ermiteconnect.NewEventServiceHandler(f)
	mount(p, h)
	p, h = ermiteconnect.NewMergeRequestServiceHandler(f)
	mount(p, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

var testNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// result is the outcome of one CLI run.
type result struct {
	stdout, stderr string
	err            error
}

// testEnv is a cliEnv with the given variables, the fixed clock, and a config
// file path inside a fresh temp dir (no file yet) so that a test never reads
// the developer's own ~/.config/ermite/config. token.cmd must not run unless
// the test sets runTokenCmd.
func testEnv(t *testing.T, vars map[string]string) *cliEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ermite", "config")
	return &cliEnv{
		getenv:     func(k string) string { return vars[k] },
		now:        func() time.Time { return testNow },
		version:    "v0.0.1",
		sleep:      func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		configPath: func() (string, error) { return path, nil },
		runTokenCmd: func(context.Context, string) (string, error) {
			t.Error("token.cmd must not run")
			return "", errors.New("token.cmd must not run")
		},
	}
}

// runCLI runs the command tree in process with the given environment.
func runCLI(t *testing.T, vars map[string]string, stdin string, args ...string) result {
	t.Helper()
	return runCLIWith(t, testEnv(t, vars), stdin, args...)
}

// runCLIWith runs the command tree in process with env.
func runCLIWith(t *testing.T, env *cliEnv, stdin string, args ...string) result {
	t.Helper()
	root := newRootCmdWith(env)
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := root.ExecuteContext(ctx)
	return result{stdout: out.String(), stderr: errb.String(), err: err}
}

func (r result) ok(t *testing.T) result {
	t.Helper()
	require.NoError(t, r.err, "stderr: %s", r.stderr)
	return r
}

func ts(s string) *timestamppb.Timestamp {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return timestamppb.New(v)
}
