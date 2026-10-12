// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

// Passkeys add an anonymous way to start a session, and the Actions pages add
// POSTs that cancel and re-run runs. The two must not meet: the only POSTs an
// anonymous browser may make are the two passkey login endpoints (and a login
// link), so the run actions still need the Origin of the public URL and a
// session, whether that session came from a link or from a passkey.
func TestPasskeysDoNotOpenTheActionsPosts(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	runs := mocks.NewRunService(t) // a call the gate should have stopped fails the test
	sessions := newSessions(t, "tok")
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t), Runs: runs,
		PublicURL: forgeOrigin, Sessions: sessions, Passkeys: mocks.NewWebPasskeyService(t), Limiter: &blockingLimiter{},
		Now: func() time.Time { return fixedNow },
	})
	f := &passkeyFixture{srv: srv}
	actions := []string{
		"/proj/actions/runs/1/cancel", "/proj/actions/runs/1/rerun", "/proj/actions/runs/1/rerun-failed",
	}
	do := func(target, origin, cookie string) *httptest.ResponseRecorder {
		return f.do(t, req{method: "POST", target: target, origin: origin, cookie: cookie, contentType: "application/x-www-form-urlencoded"})
	}

	for _, target := range actions {
		// Anonymous, with or without the right Origin, and with a cookie the
		// sessions do not know: forbidden, before any use case runs.
		for _, tc := range []struct{ origin, cookie string }{
			{forgeOrigin, ""}, {"", ""}, {forgeOrigin, "stale"},
		} {
			rec := do(target, tc.origin, tc.cookie)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s origin=%q cookie=%q", target, tc.origin, tc.cookie)
		}
		// A signed-in browser from another origin: forbidden as well.
		for _, origin := range []string{"", "null", "https://evil.example"} {
			assert.Equal(t, http.StatusForbidden, do(target, origin, "tok").Code, "%s origin=%q", target, origin)
		}
	}

	// The same paths are not passkey endpoints: lookalikes of the anonymous
	// ones do not pass either.
	for _, target := range []string{"/proj/actions/runs/1/cancel/-/passkeys/login/begin", "/-/passkeys/login/begin/../../proj/actions/runs/1/cancel"} {
		assert.NotEqual(t, http.StatusSeeOther, do(target, forgeOrigin, "").Code, target)
	}

	// A browser signed in (by a link or by a passkey, the cookie is the same)
	// reaches the use case as the owner, once per action.
	owner := mock.MatchedBy(func(p domain.Principal) bool { return p.IsOwner() })
	runs.EXPECT().CancelRun(mock.Anything, owner, repo, int64(1)).Return(domain.CIRun{}, nil).Once()
	runs.EXPECT().RerunRun(mock.Anything, owner, repo, int64(1), false).Return(domain.CIRun{}, nil).Once()
	runs.EXPECT().RerunRun(mock.Anything, owner, repo, int64(1), true).Return(domain.CIRun{}, nil).Once()
	for _, target := range actions {
		rec := do(target, forgeOrigin, "tok")
		assert.Equal(t, http.StatusSeeOther, rec.Code, target)
		assert.Equal(t, "/proj/actions/runs/1", rec.Header().Get("Location"), target)
	}
}

// The refusal of the use case still shows as the page of a forbidden action
// for a signed-in browser.
func TestPasskeySessionRefusedByTheRunUseCase(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	runs := mocks.NewRunService(t)
	runs.EXPECT().CancelRun(mock.Anything, mock.Anything, repo, int64(1)).Return(domain.CIRun{}, domain.ErrForbidden).Once()
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t), Runs: runs,
		PublicURL: forgeOrigin, Sessions: newSessions(t, "tok"), Passkeys: mocks.NewWebPasskeyService(t), Limiter: &blockingLimiter{},
	})
	f := &passkeyFixture{srv: srv}
	rec := f.do(t, req{method: "POST", target: "/proj/actions/runs/1/cancel", origin: forgeOrigin, cookie: "tok", contentType: "application/x-www-form-urlencoded"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
