// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestOwnerNavOnlyForTheSignedInOwnerWithAnInbox(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t) // never asked
	srv := &Server{cfg: Config{Inbox: svc}}
	assert.Nil(t, srv.ownerNav(t.Context(), "home"), "signed out")

	none := &Server{cfg: Config{}}
	assert.Nil(t, none.ownerNav(withOwner(t.Context()), "home"), "no inbox")
}

func TestOwnerNavFollowsTheConfig(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 2, Work: 5}, nil)

	bare := &Server{cfg: Config{Inbox: svc}}
	n := bare.ownerNav(withOwner(t.Context()), "work")
	require.NotNil(t, n)
	assert.Equal(t, OwnerNav{Current: "work", Attention: 2, Work: 5}, *n)

	full := &Server{cfg: Config{Inbox: svc, Deps: mocks.NewDepsService(t), Traffic: mocks.NewTrafficService(t)}}
	n = full.ownerNav(withOwner(t.Context()), "deps")
	require.NotNil(t, n)
	assert.True(t, n.Deps)
	assert.True(t, n.Insights)
}

func TestOwnerNavWithoutCounts(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{}, errors.New("down"))
	n := (&Server{cfg: Config{Inbox: svc}}).ownerNav(withOwner(t.Context()), "inbox")
	require.NotNil(t, n)
	assert.Equal(t, -1, n.Attention)
	assert.Equal(t, -1, n.Work)

	body := renderToString(t, OwnerNavBar(*n))
	assert.NotContains(t, body, `class="count`)
	assert.Contains(t, body, `aria-label="Inbox"`)
	assert.Contains(t, body, `aria-label="Work"`)
}

func TestOwnerNavRendersTheCurrentItemAndCounts(t *testing.T) {
	t.Parallel()
	body := renderToString(t, OwnerNavBar(OwnerNav{Current: "work", Attention: 3, Work: 0}))
	assert.Contains(t, body, `<nav id="owner-nav" class="tabs owner-nav" aria-label="Owner">`)
	assert.Contains(t, body, `href="/-/work" data-nav data-key="g w" aria-label="Work (0)" aria-current="page"`)
	assert.Equal(t, 1, strings.Count(body, `aria-current="page"`))
	assert.Contains(t, body, `<span class="count hot">3</span>`)
	assert.Contains(t, body, `<span class="count">0</span>`)
	assert.Contains(t, body, `aria-label="Inbox (3)"`)
	assert.Contains(t, body, `<span class="on-label">Repositories</span>`)
	assert.NotContains(t, body, "/-/deps")
	assert.NotContains(t, body, "/-/insights")

	all := renderToString(t, OwnerNavBar(OwnerNav{Current: "home", Deps: true, Insights: true}))
	assert.Contains(t, all, `href="/-/deps"`)
	assert.Contains(t, all, `href="/-/insights"`)
	assert.Contains(t, all, `<span class="count">0</span>`)
	assert.NotContains(t, all, "count hot")
}
