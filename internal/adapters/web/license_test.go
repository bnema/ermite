// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestLicenseID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, want string }{
		{"// SPDX-License-Identifier: MIT", "MIT"},
		{"<!-- SPDX-License-Identifier: Apache-2.0 OR MIT -->", "Apache-2.0 OR MIT"},
		{"/* SPDX-License-Identifier: GPL-3.0-or-later */", "GPL-3.0-or-later"},
		{"GNU AFFERO GENERAL PUBLIC LICENSE\nVersion 3, 19 November 2007", "AGPL-3.0"},
		{"GNU GENERAL PUBLIC LICENSE Version 3" + strings.Repeat(" padding", 100) + "GNU Affero General Public License version 3", "GPL-3.0"},
		{"GNU GENERAL PUBLIC LICENSE Version 2", "GPL-2.0"},
		{"GNU LESSER GENERAL PUBLIC LICENSE Version 2.1", "LGPL-2.1"},
		{"GNU LESSER GENERAL PUBLIC LICENSE Version 3", "LGPL-3.0"},
		{"Apache License\nVersion 2.0, January 2004", "Apache-2.0"},
		{"Mozilla Public License Version 2.0", "MPL-2.0"},
		{"Permission is hereby granted, free of charge\nTHE SOFTWARE IS PROVIDED \"AS IS\"", "MIT-0"},
		{"Redistribution and use in source and binary forms ... neither the name", "BSD-3-Clause"},
		{"Redistribution and use in source and binary forms ... this list of conditions", "BSD-2-Clause"},
		{"Redistribution and use in source and binary forms ... all advertising materials ... neither the name", "Other"},
		{"Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee", "ISC"},
		{"Custom license mentioning MIT or Apache-2.0", "Other"},
		{"", "Other"},
	} {
		t.Run(tc.want+tc.text[:min(20, len(tc.text))], func(t *testing.T) { t.Parallel(); assert.Equal(t, tc.want, licenseID(tc.text)) })
	}
}

func TestRepositoryLicenseCache(t *testing.T) {
	t.Parallel()
	for _, filename := range append(append([]string{}, licenseNames...), "LiCeNsE.MD", "missing", "directory") {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			repo := repoName(t, "proj")
			head := strings.Repeat("a", 40)
			entry := domain.TreeEntry{Name: filename, Path: filename, Kind: domain.EntryFile}
			if filename == "directory" {
				entry.Name, entry.Kind = "LICENSE", domain.EntryDirectory
			}
			browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, false).Return(domain.Tree{Entries: []domain.TreeEntry{entry}}, nil).Once()
			found := filename != "missing" && filename != "directory"
			if found {
				browse.EXPECT().OpenBlob(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything).Return(domain.BlobInfo{}, io.NopCloser(strings.NewReader("SPDX-License-Identifier: MIT")), nil).Once()
			}
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: browse})
			for range 2 {
				badge := srv.repositoryLicense(t.Context(), repo, head)
				if !found {
					assert.Nil(t, badge)
					continue
				}
				require.NotNil(t, badge)
				assert.Equal(t, "MIT", badge.ID)
				assert.Equal(t, "/proj/blob/"+head+"/"+filename, badge.Href)
			}
		})
	}
}

func TestRepositoryLicenseRetriesErrorsAndNewHeads(t *testing.T) {
	t.Parallel()
	browse := mocks.NewBrowseService(t)
	repo := repoName(t, "proj")
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, false).Return(domain.Tree{}, errors.New("unavailable")).Once()
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Twice()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: browse})
	for _, head := range []string{strings.Repeat("a", 40), strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		assert.Nil(t, srv.repositoryLicense(context.Background(), repo, head))
	}
}

func TestSummaryLicense(t *testing.T) {
	t.Parallel()
	assert.NotContains(t, renderRail(t, SummaryPage{}), "<dt>license</dt>")
	out := renderRail(t, SummaryPage{License: &LicenseBadge{ID: "MIT", Href: "/proj/blob/main/LICENSE"}})
	assert.Contains(t, out, `<dt>license</dt><dd><a href="/proj/blob/main/LICENSE">MIT</a></dd>`)
}

func TestHomeLicenseFromDefaultHead(t *testing.T) {
	t.Parallel()
	for _, binary := range []bool{false, true} {
		name := "unknown text"
		if binary {
			name = "binary"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			repo := pushedRepo(t, "proj")
			commits := homeCommits(t, 1)
			head := commits[0].ID.String()
			revision, err := domain.ParseRevision(head)
			require.NoError(t, err)
			browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo.Name, mock.Anything).Return(ports.HistoryPage{Commits: commits}, nil).Once()
			browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo.Name, commits[0].ID, mock.Anything).Return(ports.RepoStats{}, nil).Once()
			browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo.Name, revision, mock.Anything, false).Return(domain.Tree{Entries: []domain.TreeEntry{{Name: "COPYING", Path: "COPYING", Kind: domain.EntryFile}}}, nil).Once()
			browse.EXPECT().OpenBlob(mock.Anything, mock.Anything, repo.Name, revision, mock.Anything).Return(domain.BlobInfo{Binary: binary}, io.NopCloser(strings.NewReader("A custom license")), nil).Once()
			srv := newTestServer(t, Config{Repos: reposServing(t, repo), Browse: browse})
			row := srv.homeRowOf(t.Context(), repo, fixedNow, false)
			require.NotNil(t, row.License)
			assert.Equal(t, "Other", row.License.ID)
			assert.Contains(t, renderRow(t, row), `href="/proj/blob/`+head+`/COPYING" title="License: Other">Other</a>`)
			assert.Contains(t, renderRail(t, SummaryPage{License: row.License}), `<dt>license</dt><dd><a href="/proj/blob/`+head+`/COPYING">Other</a></dd>`)
		})
	}
}

func TestLicenseFullTexts(t *testing.T) {
	t.Parallel()
	const grant = `Copyright (c) 2026 Example

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so.`
	const notice = `The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.`
	const warranty = `THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.`
	const bsd = `Copyright (c) 2026 Example. All rights reserved.
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
1. Redistributions of source code must retain the above copyright notice,
   this list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.`
	const endorsement = `3. Neither the name of the copyright holder nor the names of its
   contributors may be used to endorse or promote products derived from
   this software without specific prior written permission.`
	const disclaimer = `THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
POSSIBILITY OF SUCH DAMAGE.`
	const gnu = `Copyright (C) 2007 Free Software Foundation, Inc.
Everyone is permitted to copy and distribute verbatim copies of this license
document, but changing it is not allowed.

Preamble
The licenses for most software and other practical works are designed to
take away your freedom to share and change the works. By contrast, the GNU
General Public License is intended to guarantee your freedom to share and
change all versions of a program--to make sure it remains free software
for all its users.`
	for _, tc := range []struct{ name, text, want string }{
		{"MIT", "MIT License\n" + grant + "\n" + notice + "\n" + warranty, "MIT"},
		{"MIT-0", "MIT No Attribution\n" + grant + "\n" + warranty, "MIT-0"},
		{"BSD-2", bsd + "\n" + disclaimer, "BSD-2-Clause"},
		{"BSD-3", bsd + "\n" + endorsement + "\n" + disclaimer, "BSD-3-Clause"},
		{"GPL", "GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007\n" + gnu + strings.Repeat("\nTerms and conditions for copying, distribution and modification.", 10) + "\nUse with the GNU Affero General Public License version 3.", "GPL-3.0"},
		{"LGPL", "GNU LESSER GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007\n" + gnu + "\nThis version of the GNU Lesser General Public License incorporates the terms and conditions of version 3 of the GNU General Public License, supplemented by additional permissions.", "LGPL-3.0"},
		{"AGPL", "GNU AFFERO GENERAL PUBLIC LICENSE\nVersion 3, 19 November 2007\n" + gnu + "\nNotwithstanding any other provision of this License, if you modify the Program, your modified version must prominently offer all users interacting with it remotely through a computer network an opportunity to receive the Corresponding Source.", "AGPL-3.0"},
	} {
		t.Run(tc.name, func(t *testing.T) { t.Parallel(); assert.Equal(t, tc.want, licenseID(tc.text)) })
	}
}
