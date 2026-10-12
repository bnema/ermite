// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

func twoPasskeys() *ermite.ListPasskeysResponse {
	return &ermite.ListPasskeysResponse{Passkeys: []*ermite.Passkey{
		{Id: "AQID", Name: "Pixel 9", CreatedAt: ts("2026-10-01T08:00:00Z"), LastUsedAt: ts("2026-10-09T18:30:00Z"), Synced: true},
		{Id: "BAUG", Name: "yubikey", CreatedAt: ts("2026-10-02T09:15:00Z")},
	}}
}

func TestWebPasskeyList(t *testing.T) {
	f := &fakeAPI{listPasskeys: func(*ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error) { return twoPasskeys(), nil }}
	res := runCLI(t, envFor(f.serve(t)), "", "web", "passkey", "list").ok(t)
	assert.Empty(t, res.stderr)
	assert.Regexp(t, `(?m)^ID\s+NAME\s+CREATED\s+LAST USED\s+SYNCED$`, res.stdout)
	assert.Regexp(t, `(?m)^AQID\s+Pixel 9\s+2026-10-01 08:00\s+2026-10-09 18:30\s+yes$`, res.stdout)
	assert.Regexp(t, `(?m)^BAUG\s+yubikey\s+2026-10-02 09:15\s+never\s+no$`, res.stdout)
}

func TestWebPasskeyListEmpty(t *testing.T) {
	f := &fakeAPI{listPasskeys: func(*ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error) {
		return &ermite.ListPasskeysResponse{}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "web", "passkey", "list").ok(t)
	assert.Empty(t, res.stdout)
	assert.Contains(t, res.stderr, "no passkeys")

	res = runCLI(t, envFor(url), "", "web", "passkey", "list", "--json").ok(t)
	assert.JSONEq(t, `[]`, res.stdout)
}

func TestWebPasskeyListJSON(t *testing.T) {
	f := &fakeAPI{listPasskeys: func(*ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error) { return twoPasskeys(), nil }}
	res := runCLI(t, envFor(f.serve(t)), "", "web", "passkey", "list", "--json").ok(t)
	assert.Empty(t, res.stderr)
	assert.JSONEq(t, `[
		{"id":"AQID","name":"Pixel 9","created_at":"2026-10-01T08:00:00Z","last_used_at":"2026-10-09T18:30:00Z","synced":true},
		{"id":"BAUG","name":"yubikey","created_at":"2026-10-02T09:15:00Z","last_used_at":null,"synced":false}
	]`, res.stdout)
}

func TestWebPasskeyListCleansServerText(t *testing.T) {
	f := &fakeAPI{listPasskeys: func(*ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error) {
		return &ermite.ListPasskeysResponse{Passkeys: []*ermite.Passkey{{Id: "AQ", Name: "evil\x1b[2J name", CreatedAt: ts("2026-10-01T08:00:00Z")}}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "web", "passkey", "list").ok(t)
	assert.NotContains(t, res.stdout, "\x1b")
}

func TestWebPasskeyRemove(t *testing.T) {
	f := &fakeAPI{removePasskey: func(*ermite.RemovePasskeyRequest) (*ermite.RemovePasskeyResponse, error) {
		return &ermite.RemovePasskeyResponse{}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "web", "passkey", "remove", "AQID").ok(t)
	assert.Equal(t, "removed AQID\n", res.stdout)
	assert.Contains(t, res.stderr, "every browser is signed out")

	calls, reqs := f.snapshot()
	assert.Equal(t, []string{"RemovePasskey"}, calls)
	assert.Equal(t, "AQID", reqs[0].(*ermite.RemovePasskeyRequest).GetId())
}

func TestWebPasskeyRemoveErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		code connect.Code
		msg  string
		want string
	}{
		"unknown id":    {connect.CodeNotFound, "not found", "not found"},
		"not the owner": {connect.CodePermissionDenied, "", "permission denied"},
		"unavailable":   {connect.CodeFailedPrecondition, "passkeys are not available on this server", "passkeys are not available on this server"},
		"bad id":        {connect.CodeInvalidArgument, "id is not a base64url credential id", "invalid request"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeAPI{removePasskey: func(*ermite.RemovePasskeyRequest) (*ermite.RemovePasskeyResponse, error) {
				return nil, connect.NewError(tc.code, errors.New(tc.msg))
			}}
			res := runCLI(t, envFor(f.serve(t)), "", "web", "passkey", "remove", "AQID")
			require.Error(t, res.err)
			assert.Contains(t, res.err.Error(), tc.want)
			assert.Empty(t, res.stdout, "nothing is reported as removed")
			assert.NotContains(t, res.stderr, "signed out")
		})
	}
}

func TestWebPasskeyListUnavailable(t *testing.T) {
	f := &fakeAPI{listPasskeys: func(*ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("passkeys are not available on this server"))
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "web", "passkey", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "passkeys are not available on this server")
}

func TestWebPasskeyArgs(t *testing.T) {
	f := &fakeAPI{}
	url := f.serve(t)
	for _, args := range [][]string{
		{"web", "passkey", "remove"},
		{"web", "passkey", "remove", "a", "b"},
		{"web", "passkey", "list", "extra"},
	} {
		res := runCLI(t, envFor(url), "", args...)
		require.Error(t, res.err, args)
	}
	calls, _ := f.snapshot()
	assert.Empty(t, calls, "nothing reached the server")
}

// The ids complete from the server's list.
func TestWebPasskeyRemoveCompletesIDs(t *testing.T) {
	f := &fakeAPI{listPasskeys: func(*ermite.ListPasskeysRequest) (*ermite.ListPasskeysResponse, error) { return twoPasskeys(), nil }}
	res := runCLI(t, envFor(f.serve(t)), "", "__complete", "web", "passkey", "remove", "").ok(t)
	assert.Contains(t, res.stdout, "AQID\tPixel 9")
	assert.Contains(t, res.stdout, "BAUG\tyubikey")

	res = runCLI(t, envFor(f.serve(t)), "", "__complete", "web", "passkey", "remove", "B").ok(t)
	assert.NotContains(t, res.stdout, "AQID")
	assert.Contains(t, res.stdout, "BAUG\tyubikey")
}
