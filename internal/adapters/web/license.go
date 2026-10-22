// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
)

// LicenseBadge names the root license file and its display identifier.
type LicenseBadge struct {
	ID, Href string
}

var licenseNames = []string{"license", "license.md", "license.txt", "licence", "licence.md", "licence.txt", "copying", "copying.md", "copying.txt"}

// repositoryLicense caches both absence and detection by immutable head. The
// caller has already authorized repository browsing. Failures are not cached.
// A separate instance of the bounded immutable-result cache coalesces misses.
func (s *Server) repositoryLicense(ctx context.Context, repo domain.RepoName, head string) *LicenseBadge {
	key := repo.String() + "\x00" + head
	body, err := s.licenses.load(ctx, repo.String(), key, func() ([]byte, error) {
		badge, err := s.detectLicense(ctx, repo, head)
		if err != nil {
			return nil, err
		}
		return json.Marshal(badge)
	})
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Str("repo", repo.String()).Msg("read the repository license")
		return nil
	}
	var badge *LicenseBadge
	if err := json.Unmarshal(body, &badge); err != nil {
		return nil
	}
	return badge
}

//hexcheck:ignore suspicious-business-logic-in-adapter picks a root license file for display only
func (s *Server) detectLicense(ctx context.Context, repo domain.RepoName, head string) (*LicenseBadge, error) {
	tree, err := s.getTree(ctx, repo, head, "", false)
	if err != nil {
		return nil, err
	}
	for _, name := range licenseNames {
		for _, entry := range tree.Entries {
			if (entry.Kind != domain.EntryFile && entry.Kind != domain.EntryExecutable) || !strings.EqualFold(name, entry.Name) {
				continue
			}
			body, binary, err := s.readBlob(ctx, repo, head, entry.Path, 64<<10)
			if err != nil {
				return nil, err
			}
			id := "Other"
			if !binary {
				id = licenseID(string(body))
			}
			return &LicenseBadge{ID: id, Href: repoHref(repo.String(), "blob", head, entry.Path)}, nil
		}
	}
	return nil, nil
}

// licenseID deliberately recognizes titles and characteristic grants, not
// incidental mentions of other licenses in their full text.
func licenseID(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(strings.ToLower(line), "spdx-license-identifier:"); i >= 0 {
			id := strings.TrimSpace(line[i+len("spdx-license-identifier:"):])
			id = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(id, "-->"), "*/"))
			if id != "" && len(id) <= 160 {
				return id
			}
		}
	}
	text := strings.ToLower(strings.Join(strings.Fields(body), " "))
	title := text[:min(len(text), 600)]
	switch {
	case strings.Contains(title, "gnu affero general public license") && strings.Contains(title, "version 3"):
		return "AGPL-3.0"
	case strings.Contains(title, "gnu lesser general public license") && strings.Contains(title, "version 3"):
		return "LGPL-3.0"
	case strings.Contains(title, "gnu lesser general public license") && strings.Contains(title, "version 2.1"):
		return "LGPL-2.1"
	case strings.Contains(title, "gnu general public license") && strings.Contains(title, "version 3"):
		return "GPL-3.0"
	case strings.Contains(title, "gnu general public license") && strings.Contains(title, "version 2"):
		return "GPL-2.0"
	case strings.Contains(title, "apache license") && strings.Contains(title, "version 2.0"):
		return "Apache-2.0"
	case strings.Contains(title, "mozilla public license") && strings.Contains(title, "2.0"):
		return "MPL-2.0"
	case strings.Contains(text, "permission is hereby granted, free of charge") && strings.Contains(text, "the software is provided \"as is\""):
		if strings.Contains(text, "the above copyright notice and this permission notice shall be included in all copies or substantial portions of the software") {
			return "MIT"
		}
		return "MIT-0"
	case strings.Contains(text, "all advertising materials"):
		return "Other"
	case strings.Contains(text, "redistribution and use in source and binary forms") && strings.Contains(text, "neither the name"):
		return "BSD-3-Clause"
	case strings.Contains(text, "redistribution and use in source and binary forms") && strings.Contains(text, "this list of conditions"):
		return "BSD-2-Clause"
	case strings.Contains(text, "permission to use, copy, modify, and/or distribute this software for any purpose with or without fee"):
		return "ISC"
	}
	return "Other"
}
