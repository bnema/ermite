// SPDX-License-Identifier: AGPL-3.0-or-later
package app

import (
	"bnema.dev/ermite-api/gen/ermite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditCLI(t *testing.T) {
	for _, kind := range []string{"issue", "mr"} {
		t.Run(kind, func(t *testing.T) {
			field := "body"
			if kind == "mr" {
				field = "description"
			}
			f := &fakeAPI{getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
				return &ermite.GetVersionResponse{Features: []string{"issues.edit", "merge_requests.edit"}}, nil
			}, editIssue: func(r *ermite.EditIssueRequest) (*ermite.EditIssueResponse, error) {
				return &ermite.EditIssueResponse{Issue: &ermite.Issue{Repo: r.Repo, Number: r.Number, Title: r.GetTitle(), Body: r.GetBody()}}, nil
			}, editMR: func(r *ermite.EditMergeRequestRequest) (*ermite.EditMergeRequestResponse, error) {
				return &ermite.EditMergeRequestResponse{MergeRequest: &ermite.MergeRequest{Repo: r.Repo, Number: r.Number, Title: r.GetTitle(), Description: r.GetDescription()}}, nil
			}}
			url := f.serve(t)
			res := runCLI(t, envFor(url), "", kind, "edit", "alpha", "2", "--title", "new").ok(t)
			assert.Contains(t, res.stdout, "Edited")
			file := filepath.Join(t.TempDir(), "text.md")
			require.NoError(t, os.WriteFile(file, []byte("file\n"), 0600))
			runCLI(t, envFor(url), "", kind, "edit", "alpha", "2", "--"+field+"-file", file, "--json").ok(t)
			runCLI(t, envFor(url), "", kind, "edit", "alpha", "2", "--"+field+"-file", "-").ok(t)
			_, reqs := f.snapshot()
			if kind == "issue" {
				a := reqs[1].(*ermite.EditIssueRequest)
				require.NotNil(t, a.Title)
				assert.Nil(t, a.Body)
				a = reqs[3].(*ermite.EditIssueRequest)
				assert.Nil(t, a.Title)
				assert.Equal(t, "file", a.GetBody())
				a = reqs[5].(*ermite.EditIssueRequest)
				require.NotNil(t, a.Body)
				assert.Empty(t, *a.Body)
			} else {
				a := reqs[1].(*ermite.EditMergeRequestRequest)
				require.NotNil(t, a.Title)
				assert.Nil(t, a.Description)
				a = reqs[3].(*ermite.EditMergeRequestRequest)
				assert.Nil(t, a.Title)
				assert.Equal(t, "file", a.GetDescription())
				a = reqs[5].(*ermite.EditMergeRequestRequest)
				require.NotNil(t, a.Description)
				assert.Empty(t, *a.Description)
			}
			for _, flags := range [][]string{nil, {"--" + field, "inline"}, {"--" + field + "-file", filepath.Join(t.TempDir(), "missing")}} {
				args := append([]string{kind, "edit", "alpha", "2"}, flags...)
				require.Error(t, runCLI(t, envFor(url), "", args...).err)
			}
			require.Error(t, runCLI(t, envFor(url), strings.Repeat("x", (1<<20)+1), kind, "edit", "alpha", "2", "--"+field+"-file", "-").err)
			old := versionFake()
			require.Error(t, runCLI(t, envFor(old.serve(t)), "", kind, "edit", "alpha", "2", "--title", "new").err)
		})
	}
}

func TestCreateFileOnlyText(t *testing.T) {
	for _, kind := range []string{"issue", "mr"} {
		t.Run(kind, func(t *testing.T) {
			field := "body"
			if kind == "mr" {
				field = "description"
			}
			var texts []string
			f := &fakeAPI{getVersion: withIssues,
				createIssue: func(r *ermite.CreateIssueRequest) (*ermite.CreateIssueResponse, error) {
					texts = append(texts, r.GetBody())
					return &ermite.CreateIssueResponse{Issue: issueOne}, nil
				},
				createMR: func(r *ermite.CreateMergeRequestRequest) (*ermite.CreateMergeRequestResponse, error) {
					texts = append(texts, r.GetDescription())
					return &ermite.CreateMergeRequestResponse{MergeRequest: mrOne}, nil
				},
			}
			url := f.serve(t)
			args := []string{kind, "create", "alpha", "--title", "title"}
			if kind == "mr" {
				args = append(args, "--source", "feat")
			}
			call := func(input string, flags ...string) result {
				a := append(append([]string{}, args...), flags...)
				return runCLI(t, envFor(url), input, a...)
			}
			call("ignored stdin").ok(t)
			file := filepath.Join(t.TempDir(), "text.md")
			require.NoError(t, os.WriteFile(file, []byte("from file\n"), 0600))
			call("", "--"+field+"-file", file).ok(t)
			call("from stdin\n", "--"+field+"-file", "-").ok(t)
			require.NoError(t, os.WriteFile(file, nil, 0600))
			call("", "--"+field+"-file", file).ok(t)
			assert.Equal(t, []string{"", "from file", "from stdin", ""}, texts)
			require.ErrorContains(t, call("", "--"+field, "inline").err, "unknown flag")
			require.Error(t, call("", "--"+field+"-file", filepath.Join(t.TempDir(), "missing")).err)
			require.ErrorContains(t, call(strings.Repeat("x", (1<<20)+1), "--"+field+"-file", "-").err, "1 MiB")
			require.NoError(t, os.WriteFile(file, []byte(strings.Repeat("x", (1<<20)+1)), 0600))
			require.ErrorContains(t, call("", "--"+field+"-file", file).err, "1 MiB")
		})
	}
}
