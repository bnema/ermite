// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// referenceResolver is scoped to one rendered document. Revision resolution
// goes through BrowseService so the normal repository access rules apply.
func (s *Server) referenceResolver(ctx context.Context, repo string) refResolver {
	return refResolver{numbers: s.numberResolver(ctx, repo), commits: s.commitResolver(ctx, repo), mergeRequests: true}
}

const maxCommitLookups = 50

func (s *Server) commitResolver(ctx context.Context, repo string) func(string) (string, string, bool) {
	name, err := domain.ParseRepoName(repo)
	if err != nil || s.cfg.Browse == nil {
		return nil
	}
	type answer struct{ href, label string }
	seen := map[string]answer{}
	return func(sha string) (string, string, bool) {
		sha = strings.ToLower(sha)
		if href, ok := seen[sha]; ok {
			return href.href, href.label, href.href != ""
		}
		if len(seen) >= maxCommitLookups {
			return "", "", false
		}
		a := answer{}
		rev, err := domain.ParseRevision(sha)
		if err == nil {
			resolved, err := s.cfg.Browse.ResolveRevision(ctx, principal(ctx), name, rev)
			// Reject peeled tag objects and hex-looking refs whose target differs.
			// A commit prefix shadowed by such a ref deliberately stays text.
			if err == nil && resolved.Ref == "" && !resolved.CommitID.IsZero() && strings.HasPrefix(strings.ToLower(resolved.CommitID.String()), sha) {
				full := resolved.CommitID.String()
				a = answer{repoHref(repo, "commit", full), full[:7]}
			}
		}
		seen[sha] = a
		return a.href, a.label, a.href != ""
	}
}

func (s *Server) noteResolver(ctx context.Context, repo string, comments []domain.Comment) refResolver {
	res := s.referenceResolver(ctx, repo)
	res.notes = noteIDs(comments)
	return res
}
