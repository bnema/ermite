// SPDX-License-Identifier: AGPL-3.0-or-later

package githubraw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/githubraw"
	"bnema.dev/ermite/internal/domain"
)

// The server is local: the test never reaches github.com.
func TestFetchWorkflow(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/ok.yml"):
			_, _ = w.Write([]byte("on: workflow_call\n"))
		case strings.HasSuffix(r.URL.Path, "/big.yml"):
			_, _ = w.Write([]byte(strings.Repeat("x", 100)))
		case strings.HasSuffix(r.URL.Path, "/moved.yml"):
			http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/boom.yml"):
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f := githubraw.New(githubraw.Config{BaseURL: srv.URL})
	ctx := context.Background()

	data, err := f.FetchWorkflow(ctx, "o", "r", "abc", ".github/workflows/ok.yml", 1024)
	require.NoError(t, err)
	assert.Equal(t, "on: workflow_call\n", string(data))
	assert.Equal(t, "/o/r/abc/.github/workflows/ok.yml", got)

	_, err = f.FetchWorkflow(ctx, "o", "r", "abc", ".github/workflows/none.yml", 1024)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.FetchWorkflow(ctx, "o", "r", "abc", ".github/workflows/big.yml", 10)
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	_, err = f.FetchWorkflow(ctx, "o", "r", "abc", ".github/workflows/boom.yml", 10)
	assert.ErrorIs(t, err, domain.ErrUnavailable)
	_, err = f.FetchWorkflow(ctx, "o", "r", "abc", ".github/workflows/moved.yml", 10)
	assert.ErrorIs(t, err, domain.ErrUnavailable, "a redirect is not followed")
	_, err = f.FetchWorkflow(ctx, "o", "..", "abc", ".github/workflows/ok.yml", 10)
	assert.ErrorIs(t, err, domain.ErrInvalid)
}
