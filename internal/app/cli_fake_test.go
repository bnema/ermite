// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"connectrpc.com/connect/v2/connecthttp"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
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
	ermiteconnect.UnimplementedIssueServiceHandler
	ermiteconnect.UnimplementedSessionServiceHandler
	ermiteconnect.UnimplementedRunServiceHandler
	ermiteconnect.UnimplementedTrustedKeyServiceHandler
	ermiteconnect.UnimplementedDepsServiceHandler
	ermiteconnect.UnimplementedJournalServiceHandler
	ermiteconnect.UnimplementedReleaseServiceHandler

	runners []*ermite.Runner // ListRunners answer; nil means one default runner

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
	watch       func(context.Context, *ermite.WatchRequest, ermiteconnect.EventServiceWatchServerStream) error

	editMR        func(*ermite.EditMergeRequestRequest) (*ermite.EditMergeRequestResponse, error)
	editIssue     func(*ermite.EditIssueRequest) (*ermite.EditIssueResponse, error)
	createMR      func(*ermite.CreateMergeRequestRequest) (*ermite.CreateMergeRequestResponse, error)
	listMR        func(*ermite.ListMergeRequestsRequest) (*ermite.ListMergeRequestsResponse, error)
	getMR         func(*ermite.GetMergeRequestRequest) (*ermite.GetMergeRequestResponse, error)
	addComment    func(*ermite.AddCommentRequest) (*ermite.AddCommentResponse, error)
	resolveCmt    func(*ermite.ResolveCommentRequest) (*ermite.ResolveCommentResponse, error)
	closeMR       func(*ermite.CloseMergeRequestRequest) (*ermite.CloseMergeRequestResponse, error)
	mergeMR       func(*ermite.MergeMergeRequestRequest) (*ermite.MergeMergeRequestResponse, error)
	markRead      func(*ermite.MarkReadRequest) (*ermite.MarkReadResponse, error)
	enableAuto    func(*ermite.EnableAutoMergeRequest) (*ermite.EnableAutoMergeResponse, error)
	cancelAuto    func(*ermite.CancelAutoMergeRequest) (*ermite.CancelAutoMergeResponse, error)
	createIssue   func(*ermite.CreateIssueRequest) (*ermite.CreateIssueResponse, error)
	listIssues    func(*ermite.ListIssuesRequest) (*ermite.ListIssuesResponse, error)
	getIssue      func(*ermite.GetIssueRequest) (*ermite.GetIssueResponse, error)
	addIssueCmt   func(*ermite.AddIssueCommentRequest) (*ermite.AddIssueCommentResponse, error)
	resolveIssue  func(*ermite.ResolveIssueRequest) (*ermite.ResolveIssueResponse, error)
	closeIssue    func(*ermite.CloseIssueRequest) (*ermite.CloseIssueResponse, error)
	reopenIssue   func(*ermite.ReopenIssueRequest) (*ermite.ReopenIssueResponse, error)
	getSigningKey func(*ermite.GetSigningKeyRequest) (*ermite.GetSigningKeyResponse, error)

	listPasskeys  func(*ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error)
	removePasskey func(*ermite.RemovePasskeyRequest) (*ermite.RemovePasskeyResponse, error)

	listTrustedKeys  func(*ermite.ListTrustedKeysRequest) (*ermite.ListTrustedKeysResponse, error)
	addTrustedKey    func(*ermite.AddTrustedKeyRequest) (*ermite.AddTrustedKeyResponse, error)
	removeTrustedKey func(*ermite.RemoveTrustedKeyRequest) (*ermite.RemoveTrustedKeyResponse, error)

	listDeps   func(*ermite.ListDependencyUpdatesRequest) (*ermite.ListDependencyUpdatesResponse, error)
	checkDeps  func(*ermite.CheckDependenciesRequest) (*ermite.CheckDependenciesResponse, error)
	openDeps   func(*ermite.OpenDependencyUpdatesRequest) (*ermite.OpenDependencyUpdatesResponse, error)
	ignoreDeps func(*ermite.IgnoreDependencyUpdatesRequest) (*ermite.IgnoreDependencyUpdatesResponse, error)
	snoozeDeps func(*ermite.SnoozeDependencyUpdatesRequest) (*ermite.SnoozeDependencyUpdatesResponse, error)
	autoDeps   func(*ermite.SetDependencyAutoOpenRequest) (*ermite.SetDependencyAutoOpenResponse, error)
	splitDeps  func(*ermite.SplitDependencyRequest) (*ermite.SplitDependencyResponse, error)

	listActivity   func(*ermite.ListActivityRequest) (*ermite.ListActivityResponse, error)
	listUnreleased func(*ermite.ListUnreleasedRequest) (*ermite.ListUnreleasedResponse, error)
}

func (f *fakeAPI) ListActivity(ctx context.Context, r *ermite.ListActivityRequest) (*ermite.ListActivityResponse, error) {
	return respond(ctx, f, "ListActivity", r, f.listActivity)
}
func (f *fakeAPI) ListUnreleased(ctx context.Context, r *ermite.ListUnreleasedRequest) (*ermite.ListUnreleasedResponse, error) {
	return respond(ctx, f, "ListUnreleased", r, f.listUnreleased)
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

func respond[Req, Res any](ctx context.Context, f *fakeAPI, name string, req *Req, fn func(*Req) (*Res, error)) (*Res, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	f.record(name, headerHTTP(info.RequestHeader()), req)
	if fn == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "")
	}
	res, err := fn(req)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (f *fakeAPI) ListRepos(ctx context.Context, r *ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
	return respond(ctx, f, "ListRepos", r, f.listRepos)
}
func (f *fakeAPI) GetRepo(ctx context.Context, r *ermite.GetRepoRequest) (*ermite.GetRepoResponse, error) {
	return respond(ctx, f, "GetRepo", r, f.getRepo)
}
func (f *fakeAPI) CreateRepo(ctx context.Context, r *ermite.CreateRepoRequest) (*ermite.CreateRepoResponse, error) {
	return respond(ctx, f, "CreateRepo", r, f.createRepo)
}
func (f *fakeAPI) UpdateRepo(ctx context.Context, r *ermite.UpdateRepoRequest) (*ermite.UpdateRepoResponse, error) {
	return respond(ctx, f, "UpdateRepo", r, f.updateRepo)
}
func (f *fakeAPI) DeleteRepo(ctx context.Context, r *ermite.DeleteRepoRequest) (*ermite.DeleteRepoResponse, error) {
	return respond(ctx, f, "DeleteRepo", r, f.deleteRepo)
}
func (f *fakeAPI) CreateToken(ctx context.Context, r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
	return respond(ctx, f, "CreateToken", r, f.createToken)
}
func (f *fakeAPI) ListTokens(ctx context.Context, r *ermite.ListTokensRequest) (*ermite.ListTokensResponse, error) {
	return respond(ctx, f, "ListTokens", r, f.listTokens)
}
func (f *fakeAPI) RevokeToken(ctx context.Context, r *ermite.RevokeTokenRequest) (*ermite.RevokeTokenResponse, error) {
	return respond(ctx, f, "RevokeToken", r, f.revokeToken)
}
func (f *fakeAPI) GetVersion(ctx context.Context, r *ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
	return respond(ctx, f, "GetVersion", r, f.getVersion)
}
func (f *fakeAPI) CreateMergeRequest(ctx context.Context, r *ermite.CreateMergeRequestRequest) (*ermite.CreateMergeRequestResponse, error) {
	return respond(ctx, f, "CreateMergeRequest", r, f.createMR)
}
func (f *fakeAPI) ListMergeRequests(ctx context.Context, r *ermite.ListMergeRequestsRequest) (*ermite.ListMergeRequestsResponse, error) {
	return respond(ctx, f, "ListMergeRequests", r, f.listMR)
}
func (f *fakeAPI) GetMergeRequest(ctx context.Context, r *ermite.GetMergeRequestRequest) (*ermite.GetMergeRequestResponse, error) {
	return respond(ctx, f, "GetMergeRequest", r, f.getMR)
}
func (f *fakeAPI) AddComment(ctx context.Context, r *ermite.AddCommentRequest) (*ermite.AddCommentResponse, error) {
	return respond(ctx, f, "AddComment", r, f.addComment)
}
func (f *fakeAPI) ResolveComment(ctx context.Context, r *ermite.ResolveCommentRequest) (*ermite.ResolveCommentResponse, error) {
	return respond(ctx, f, "ResolveComment", r, f.resolveCmt)
}
func (f *fakeAPI) CloseMergeRequest(ctx context.Context, r *ermite.CloseMergeRequestRequest) (*ermite.CloseMergeRequestResponse, error) {
	return respond(ctx, f, "CloseMergeRequest", r, f.closeMR)
}
func (f *fakeAPI) MergeMergeRequest(ctx context.Context, r *ermite.MergeMergeRequestRequest) (*ermite.MergeMergeRequestResponse, error) {
	return respond(ctx, f, "MergeMergeRequest", r, f.mergeMR)
}
func (f *fakeAPI) EnableAutoMerge(ctx context.Context, r *ermite.EnableAutoMergeRequest) (*ermite.EnableAutoMergeResponse, error) {
	return respond(ctx, f, "EnableAutoMerge", r, f.enableAuto)
}
func (f *fakeAPI) CancelAutoMerge(ctx context.Context, r *ermite.CancelAutoMergeRequest) (*ermite.CancelAutoMergeResponse, error) {
	return respond(ctx, f, "CancelAutoMerge", r, f.cancelAuto)
}
func (f *fakeAPI) MarkRead(ctx context.Context, r *ermite.MarkReadRequest) (*ermite.MarkReadResponse, error) {
	return respond(ctx, f, "MarkRead", r, f.markRead)
}
func (f *fakeAPI) CreateIssue(ctx context.Context, r *ermite.CreateIssueRequest) (*ermite.CreateIssueResponse, error) {
	return respond(ctx, f, "CreateIssue", r, f.createIssue)
}
func (f *fakeAPI) ListIssues(ctx context.Context, r *ermite.ListIssuesRequest) (*ermite.ListIssuesResponse, error) {
	return respond(ctx, f, "ListIssues", r, f.listIssues)
}
func (f *fakeAPI) GetIssue(ctx context.Context, r *ermite.GetIssueRequest) (*ermite.GetIssueResponse, error) {
	return respond(ctx, f, "GetIssue", r, f.getIssue)
}
func (f *fakeAPI) AddIssueComment(ctx context.Context, r *ermite.AddIssueCommentRequest) (*ermite.AddIssueCommentResponse, error) {
	return respond(ctx, f, "AddIssueComment", r, f.addIssueCmt)
}
func (f *fakeAPI) CloseIssue(ctx context.Context, r *ermite.CloseIssueRequest) (*ermite.CloseIssueResponse, error) {
	return respond(ctx, f, "CloseIssue", r, f.closeIssue)
}
func (f *fakeAPI) ResolveIssue(ctx context.Context, r *ermite.ResolveIssueRequest) (*ermite.ResolveIssueResponse, error) {
	return respond(ctx, f, "ResolveIssue", r, f.resolveIssue)
}
func (f *fakeAPI) ReopenIssue(ctx context.Context, r *ermite.ReopenIssueRequest) (*ermite.ReopenIssueResponse, error) {
	return respond(ctx, f, "ReopenIssue", r, f.reopenIssue)
}
func (f *fakeAPI) GetSigningKey(ctx context.Context, r *ermite.GetSigningKeyRequest) (*ermite.GetSigningKeyResponse, error) {
	return respond(ctx, f, "GetSigningKey", r, f.getSigningKey)
}
func (f *fakeAPI) ListPasskeys(ctx context.Context, r *ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error) {
	return respond(ctx, f, "ListPasskeys", r, f.listPasskeys)
}
func (f *fakeAPI) RemovePasskey(ctx context.Context, r *ermite.RemovePasskeyRequest) (*ermite.RemovePasskeyResponse, error) {
	return respond(ctx, f, "RemovePasskey", r, f.removePasskey)
}
func (f *fakeAPI) ListTrustedKeys(ctx context.Context, r *ermite.ListTrustedKeysRequest) (*ermite.ListTrustedKeysResponse, error) {
	return respond(ctx, f, "ListTrustedKeys", r, f.listTrustedKeys)
}
func (f *fakeAPI) AddTrustedKey(ctx context.Context, r *ermite.AddTrustedKeyRequest) (*ermite.AddTrustedKeyResponse, error) {
	return respond(ctx, f, "AddTrustedKey", r, f.addTrustedKey)
}
func (f *fakeAPI) RemoveTrustedKey(ctx context.Context, r *ermite.RemoveTrustedKeyRequest) (*ermite.RemoveTrustedKeyResponse, error) {
	return respond(ctx, f, "RemoveTrustedKey", r, f.removeTrustedKey)
}
func (f *fakeAPI) Watch(ctx context.Context, r *ermite.WatchRequest, st ermiteconnect.EventServiceWatchServerStream) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	f.record("Watch", headerHTTP(info.RequestHeader()), r)
	if f.watch == nil {
		return connect.NewError(connect.CodeUnimplemented, "")
	}
	return f.watch(ctx, r, st)
}

// serve starts the fake under /-/api and returns its base URL.
func (f *fakeAPI) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	server := connect.NewServer()
	ermiteconnect.RegisterRepoServiceHandler(server, f)
	ermiteconnect.RegisterTokenServiceHandler(server, f)
	ermiteconnect.RegisterServerServiceHandler(server, f)
	ermiteconnect.RegisterEventServiceHandler(server, f)
	ermiteconnect.RegisterMergeRequestServiceHandler(server, f)
	ermiteconnect.RegisterIssueServiceHandler(server, f)
	ermiteconnect.RegisterSessionServiceHandler(server, f)
	ermiteconnect.RegisterRunServiceHandler(server, f)
	ermiteconnect.RegisterTrustedKeyServiceHandler(server, f)
	ermiteconnect.RegisterDepsServiceHandler(server, f)
	ermiteconnect.RegisterJournalServiceHandler(server, f)
	ermiteconnect.RegisterReleaseServiceHandler(server, f)
	sub := http.NewServeMux()
	connecthttp.Mount(sub, server, connecthttp.WithReadMaxBytes(0))
	mux.Handle("/-/api/", http.StripPrefix("/-/api", sub))
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

func (f *fakeAPI) EditIssue(ctx context.Context, r *ermite.EditIssueRequest) (*ermite.EditIssueResponse, error) {
	return respond(ctx, f, "EditIssue", r, f.editIssue)
}
func (f *fakeAPI) EditMergeRequest(ctx context.Context, r *ermite.EditMergeRequestRequest) (*ermite.EditMergeRequestResponse, error) {
	return respond(ctx, f, "EditMergeRequest", r, f.editMR)
}

func (f *fakeAPI) ListDependencyUpdates(ctx context.Context, r *ermite.ListDependencyUpdatesRequest) (*ermite.ListDependencyUpdatesResponse, error) {
	return respond(ctx, f, "ListDependencyUpdates", r, f.listDeps)
}
func (f *fakeAPI) CheckDependencies(ctx context.Context, r *ermite.CheckDependenciesRequest) (*ermite.CheckDependenciesResponse, error) {
	return respond(ctx, f, "CheckDependencies", r, f.checkDeps)
}
func (f *fakeAPI) OpenDependencyUpdates(ctx context.Context, r *ermite.OpenDependencyUpdatesRequest) (*ermite.OpenDependencyUpdatesResponse, error) {
	return respond(ctx, f, "OpenDependencyUpdates", r, f.openDeps)
}
func (f *fakeAPI) IgnoreDependencyUpdates(ctx context.Context, r *ermite.IgnoreDependencyUpdatesRequest) (*ermite.IgnoreDependencyUpdatesResponse, error) {
	return respond(ctx, f, "IgnoreDependencyUpdates", r, f.ignoreDeps)
}
func (f *fakeAPI) SetDependencyAutoOpen(ctx context.Context, r *ermite.SetDependencyAutoOpenRequest) (*ermite.SetDependencyAutoOpenResponse, error) {
	return respond(ctx, f, "SetDependencyAutoOpen", r, f.autoDeps)
}
func (f *fakeAPI) SplitDependency(ctx context.Context, r *ermite.SplitDependencyRequest) (*ermite.SplitDependencyResponse, error) {
	return respond(ctx, f, "SplitDependency", r, f.splitDeps)
}
func (f *fakeAPI) SnoozeDependencyUpdates(ctx context.Context, r *ermite.SnoozeDependencyUpdatesRequest) (*ermite.SnoozeDependencyUpdatesResponse, error) {
	return respond(ctx, f, "SnoozeDependencyUpdates", r, f.snoozeDeps)
}

func headerHTTP(h *connect.Header) http.Header {
	out := make(http.Header)
	for k, v := range h.All() {
		out[k] = v
	}
	return out
}
