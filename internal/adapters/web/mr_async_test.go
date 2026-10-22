// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func asyncMergeServer(t *testing.T, state domain.MRState) (*Server, *mocks.MergeRequestService) {
	t.Helper()
	svc := mocks.NewMergeRequestService(t)
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RefPage{Refs: []domain.Ref{{Kind: domain.RefKindBranch, ShortName: "feature"}}}, nil).Maybe()
	svc.EXPECT().Get(mock.Anything, mock.Anything, repoName(t, "demo"), int64(3), mock.Anything).Return(ports.MergeRequestDetail{
		MergeRequest: domain.MergeRequest{Repo: repoName(t, "demo"), Number: 3, State: state, HeadID: objectID(t, strings.Repeat("a", domain.ObjectIDLen)), Source: "feature", Target: "main"},
		Mergeability: domain.Mergeability{State: domain.MergeabilityUpToDate},
	}, nil).Maybe()
	return newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "demo")), Browse: browse, MergeRequests: svc, Now: time.Now}), svc
}

func TestAsyncMergePost(t *testing.T) {
	for _, mode := range []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash, domain.MergeModeMerge} {
		for _, hx := range []bool{false, true} {
			t.Run(mode.String()+"/"+boolText(hx), func(t *testing.T) {
				s, svc := asyncMergeServer(t, domain.MRStateMerged)
				svc.EXPECT().Merge(mock.Anything, mock.Anything, repoName(t, "demo"), int64(3), ports.MergeParams{Mode: mode}).Return(ports.MergeResult{}, nil).Once()
				r := autoMergeRequest("/demo/merge-requests/3/merge", "mode="+mode.String())
				if hx {
					r.Header.Set("HX-Request", "true")
				}
				w := httptest.NewRecorder()
				s.mrAction(s.mrMerge)(w, r)
				if !hx {
					assert.Equal(t, http.StatusSeeOther, w.Code)
					return
				}
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Empty(t, w.Header().Get("Location"))
				assert.Contains(t, w.Body.String(), `id="mr-state"`)
				assert.Contains(t, w.Body.String(), `data-state="merged"`)
				assert.Contains(t, w.Body.String(), `id="mr-actions"`)
				assert.NotContains(t, w.Body.String(), "<html")
				assert.NotContains(t, w.Body.String(), `id="merge-menu"`)
			})
		}
	}
}

func TestAsyncMergeRefusal(t *testing.T) {
	s, svc := asyncMergeServer(t, domain.MRStateOpen)
	svc.EXPECT().Merge(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(ports.MergeResult{}, domain.ErrConflict)
	r := autoMergeRequest("/demo/merge-requests/3/merge", "mode=squash")
	r = r.WithContext(context.WithValue(r.Context(), signedInKey{}, struct{}{}))
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	s.mrAction(s.mrMerge)(w, r)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `role="alert"`)
	assert.Contains(t, w.Body.String(), domain.ErrConflict.Error())
	assert.Contains(t, w.Body.String(), `hx-post="/demo/merge-requests/3/merge"`)
	assert.Contains(t, w.Body.String(), `hx-sync="this:drop"`)
	assert.Contains(t, w.Body.String(), "Merging…")
	assert.NotContains(t, w.Body.String(), "<html")
}

func TestAsyncMergeLive(t *testing.T) {
	s, _ := asyncMergeServer(t, domain.MRStateMerged)
	l := liveStream{s: s, repo: "demo", view: "mr", n: "3"}
	fs, err := l.repoPage(t.Context(), watchOf(domain.MergeRequestMerged{MergeRequest: domain.MergeRequest{Number: 3}}))
	require.NoError(t, err)
	require.Len(t, fs, 1)
	assert.Equal(t, "mr-mergeability", fs[0].event)
	assert.Contains(t, fs[0].html, `data-state="merged"`)
	assert.Contains(t, fs[0].html, `id="mr-actions"`)
	assert.False(t, strings.Contains(fs[0].html, "reload"))
	fs, err = l.repoPage(t.Context(), watchOf(domain.MergeRequestMerged{MergeRequest: domain.MergeRequest{Number: 4}}))
	require.NoError(t, err)
	assert.Empty(t, fs)
}

func TestAsyncMergeOtherFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mergeErr  error
		readFails bool
		status    int
		message   string
	}{
		{"forbidden", domain.ErrForbidden, false, http.StatusForbidden, "the session is not allowed"},
		{"internal", errors.New("private backend detail"), false, http.StatusInternalServerError, "reload to check its state"},
		{"post-merge read", nil, true, http.StatusOK, "merged, but the updated page could not be loaded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, svc := asyncMergeServer(t, domain.MRStateMerged)
			if tt.readFails {
				svc.On("Get", mock.Anything, mock.Anything, repoName(t, "demo"), int64(3), mock.Anything).Unset()
				svc.EXPECT().Get(mock.Anything, mock.Anything, repoName(t, "demo"), int64(3), mock.Anything).Return(ports.MergeRequestDetail{}, errors.New("private read detail")).Once()
			}
			svc.EXPECT().Merge(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(ports.MergeResult{}, tt.mergeErr).Once()
			r := autoMergeRequest("/demo/merge-requests/3/merge", "mode=squash")
			r.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			s.mrAction(s.mrMerge)(w, r)
			assert.Equal(t, tt.status, w.Code)
			assert.Contains(t, w.Body.String(), `id="mr-actions"`)
			assert.Contains(t, w.Body.String(), `hx-swap-oob="true"`)
			assert.Contains(t, w.Body.String(), `role="alert"`)
			assert.Contains(t, w.Body.String(), tt.message)
			assert.NotContains(t, w.Body.String(), "private")
			assert.NotContains(t, w.Body.String(), "<html")
		})
	}
}

func TestMergeGateRefusalFragment(t *testing.T) {
	s, _ := newSessionServer(t, newSessions(t, "tok"), testPublicURL)
	for _, origin := range []string{testPublicURL, "https://other.example"} {
		for _, hx := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodPost, "/demo/merge-requests/3/merge", strings.NewReader("mode=squash"))
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if hx {
				r.Header.Set("HX-Request", "true")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			assert.Equal(t, http.StatusForbidden, w.Code)
			if hx {
				assert.Contains(t, w.Body.String(), `hx-swap-oob="true"`)
				assert.Contains(t, w.Body.String(), `id="mr-actions"`)
				if origin == testPublicURL {
					assert.Contains(t, w.Body.String(), "your session has expired")
				} else {
					assert.Contains(t, w.Body.String(), "origin was not accepted")
				}
			} else {
				assert.NotContains(t, w.Body.String(), "hx-swap-oob")
			}
		}
	}
}

func TestNonHXMergeRefusalHasOneAlert(t *testing.T) {
	s, svc := asyncMergeServer(t, domain.MRStateMerged)
	svc.EXPECT().CountOpen(mock.Anything, mock.Anything, mock.Anything).Return(0, nil)
	svc.EXPECT().Merge(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(ports.MergeResult{}, domain.ErrConflict).Once()
	r := autoMergeRequest("/demo/merge-requests/3/merge", "mode=squash")
	w := httptest.NewRecorder()
	s.mrAction(s.mrMerge)(w, r)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, 1, strings.Count(w.Body.String(), `role="alert"`))
}
