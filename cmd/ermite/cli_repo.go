// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

const (
	visibilityPublic  = "public"
	visibilityPrivate = "private"
)

// jsonRepo is the stable JSON shape of a repository.
type jsonRepo struct {
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	Visibility    string  `json:"visibility"`
	DefaultBranch string  `json:"default_branch"`
	CreatedAt     *string `json:"created_at"`
	PushedAt      *string `json:"pushed_at"`
	// MergeModes are the allowed merge modes ("fast-forward", "squash").
	MergeModes         []string `json:"merge_modes"`
	FailingChecksBlock bool     `json:"failing_checks_block"`
}

func toJSONRepo(r *ermite.Repo) jsonRepo {
	modes := []string{}
	for _, m := range r.GetSettings().GetAllowedMergeModes() {
		modes = append(modes, modeName(m))
	}
	return jsonRepo{
		MergeModes:         modes,
		FailingChecksBlock: r.GetSettings().GetFailingChecksBlock(),
		Name:               r.GetName(),
		Description:        r.GetDescription(),
		Visibility:         visibilityName(r.GetVisibility()),
		DefaultBranch:      r.GetDefaultBranch(),
		CreatedAt:          jsonTime(r.GetCreatedAt()),
		PushedAt:           jsonTime(r.GetPushedAt()),
	}
}

func visibilityName(v ermite.Visibility) string {
	switch v {
	case ermite.Visibility_VISIBILITY_PUBLIC:
		return visibilityPublic
	case ermite.Visibility_VISIBILITY_PRIVATE:
		return visibilityPrivate
	default:
		return "unknown"
	}
}

func parseVisibility(s string) (ermite.Visibility, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case visibilityPublic:
		return ermite.Visibility_VISIBILITY_PUBLIC, nil
	case visibilityPrivate:
		return ermite.Visibility_VISIBILITY_PRIVATE, nil
	default:
		return 0, fmt.Errorf("invalid visibility %q: use %s or %s", s, visibilityPublic, visibilityPrivate)
	}
}

const jsonRepoHelp = `
JSON fields of a repository (--json): name, description, visibility ("public" or
"private"), default_branch, created_at and pushed_at (RFC 3339 UTC; pushed_at is
null until the first push), merge_modes (array of the allowed merge modes, "fast-forward" and/or
"squash") and failing_checks_block (whether a failing check prevents a merge).`

func newRepoCmd(env *cliEnv) *cobra.Command {
	repo := &cobra.Command{
		Use:   "repo",
		Short: "Manage repositories on a remote server",
		Long:  "Manage repositories on the server at ERMITE_URL, authenticated with ERMITE_TOKEN.",
	}
	repo.AddCommand(
		newRepoListCmd(env), newRepoShowCmd(env), newRepoCreateCmd(env),
		newRepoEditCmd(env), newRepoDeleteCmd(env),
	)
	return repo
}

// listRepos reads every page of repositories.
func listRepos(ctx context.Context, c *client.Client) ([]*ermite.Repo, error) {
	return collect(ctx, func(token string) ([]*ermite.Repo, string, error) {
		res, err := c.Repos().ListRepos(ctx, connect.NewRequest(&ermite.ListReposRequest{PageToken: token}))
		if err != nil {
			return nil, "", c.Describe(err)
		}
		return res.Msg.GetRepos(), res.Msg.GetNextPageToken(), nil
	})
}

// completeRepoNames completes the first argument with repository names. Any
// failure, including a missing configuration, silently completes nothing.
func completeRepoNames(env *cliEnv) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return repoNames(env, cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// repoNames lists the repository names starting with prefix, with their
// visibility and description as completion hints.
func repoNames(env *cliEnv, cmd *cobra.Command, prefix string) []string {
	return complete(env, cmd, prefix, listRepos, func(r *ermite.Repo) (string, string) {
		desc := visibilityName(r.GetVisibility())
		if d := clean(r.GetDescription()); d != "" {
			desc += ", " + d
		}
		return r.GetName(), desc
	})
}

// outputRepo prints a repository as JSON or as fields.
func outputRepo(cmd *cobra.Command, r *ermite.Repo, asJSON bool) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONRepo(r))
	}
	return printRepo(cmd.OutOrStdout(), r)
}

func newRepoListCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the repositories you can see",
		Long:  "List the repositories you can see: all public ones, and the private ones your token may read." + jsonRepoHelp + "\n--json prints an array of repositories ordered by name.",
		Args:  cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		repos, err := listRepos(env.ctx(cmd), c)
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonRepo, 0, len(repos))
			for _, r := range repos {
				out = append(out, toJSONRepo(r))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(repos) == 0 {
			noticef(cmd, "no repositories")
			return nil
		}
		rows := make([][]string, 0, len(repos))
		for _, r := range repos {
			rows = append(rows, []string{
				r.GetName(), visibilityName(r.GetVisibility()), r.GetDefaultBranch(),
				shortTime(r.GetPushedAt(), "never"), r.GetDescription(),
			})
		}
		return table(cmd.OutOrStdout(), []string{"NAME", "VISIBILITY", "BRANCH", "LAST PUSH", "DESCRIPTION"}, rows)
	}
	return cmd
}

func printRepo(w io.Writer, r *ermite.Repo) error {
	desc := r.GetDescription()
	if desc == "" {
		desc = "-"
	}
	modes := toJSONRepo(r).MergeModes
	return fields(w, [][2]string{
		{"name", r.GetName()},
		{"visibility", visibilityName(r.GetVisibility())},
		{"default branch", r.GetDefaultBranch()},
		{"description", desc},
		{"created", shortTime(r.GetCreatedAt(), "-")},
		{"last push", shortTime(r.GetPushedAt(), "never")},
		{"merge modes", orDash(strings.Join(modes, ", "))},
		{"failing checks block", strconv.FormatBool(r.GetSettings().GetFailingChecksBlock())},
	})
}

func newRepoShowCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "show <name>",
		Short:             "Show one repository",
		Long:              "Show one repository." + jsonRepoHelp,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.Repos().GetRepo(env.ctx(cmd), connect.NewRequest(&ermite.GetRepoRequest{Repo: args[0]}))
		if err != nil {
			return c.Describe(err)
		}
		return outputRepo(cmd, res.Msg.GetRepo(), *asJSON)
	}
	return cmd
}

func newRepoCreateCmd(env *cliEnv) *cobra.Command {
	var (
		description string
		visibility  string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an empty repository",
		Long: "Create an empty repository. It is private unless --visibility public is given. Names are\n" +
			"lowercase, 1-100 characters from [a-z0-9._-]. Needs the repo:write scope." + jsonRepoHelp,
		Args: cobra.ExactArgs(1),
	}
	cmd.Flags().StringVar(&description, "description", "", "one-line description")
	cmd.Flags().StringVar(&visibility, "visibility", visibilityPrivate, "visibility: public or private")
	_ = cmd.RegisterFlagCompletionFunc("visibility", visibilityCompletion)
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		req, err := buildCreateRepo(args[0], description, visibility)
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.Repos().CreateRepo(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		return outputRepo(cmd, res.Msg.GetRepo(), *asJSON)
	}
	return cmd
}

func visibilityCompletion(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return []string{visibilityPublic, visibilityPrivate}, cobra.ShellCompDirectiveNoFileComp
}

// buildCreateRepo maps the flags of `repo create` to the request.
func buildCreateRepo(name, description, visibility string) (*ermite.CreateRepoRequest, error) {
	vis, err := parseVisibility(visibility)
	if err != nil {
		return nil, err
	}
	return &ermite.CreateRepoRequest{Name: name, Description: description, Visibility: vis}, nil
}

func newRepoEditCmd(env *cliEnv) *cobra.Command {
	var description, visibility, defaultBranch string
	var mergeModes []string
	var failingChecksBlock bool
	cmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Change a repository's description, visibility, default branch or merge settings",
		Long: "Change the fields given as flags and leave the others alone; --description \"\" clears the\n" +
			"description. The default branch must already exist. --merge-modes lists the modes a merge request\n" +
			"may be merged with (fast-forward, squash; default for a new repository: both), and\n" +
			"--failing-checks-block says whether a failing check prevents a merge (there are no checks yet).\n" +
			"Needs the repo:admin scope." + jsonRepoHelp,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&description, "description", "", "new description (empty clears it)")
	cmd.Flags().StringVar(&visibility, "visibility", "", "new visibility: public or private")
	cmd.Flags().StringVar(&defaultBranch, "default-branch", "", "short name of the new default branch")
	cmd.Flags().StringSliceVar(&mergeModes, "merge-modes", nil, "allowed merge modes, comma-separated: fast-forward, squash")
	cmd.Flags().BoolVar(&failingChecksBlock, "failing-checks-block", true, "a failing check prevents a merge (--failing-checks-block=false to allow it)")
	_ = cmd.RegisterFlagCompletionFunc("visibility", visibilityCompletion)
	_ = cmd.RegisterFlagCompletionFunc("merge-modes", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{modeFastForward, modeSquash}, cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		req, err := buildUpdateRepo(cmd, args[0], description, visibility, defaultBranch, mergeModes, failingChecksBlock)
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.Repos().UpdateRepo(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		return outputRepo(cmd, res.Msg.GetRepo(), *asJSON)
	}
	return cmd
}

// buildUpdateRepo maps the flags of `repo edit` to the request: only the flags
// the user passed go into the update mask.
func buildUpdateRepo(cmd *cobra.Command, name, description, visibility, defaultBranch string, mergeModes []string, failingChecksBlock bool) (*ermite.UpdateRepoRequest, error) {
	req := &ermite.UpdateRepoRequest{Repo: name}
	var paths []string
	if cmd.Flags().Changed("description") {
		req.Description = description
		paths = append(paths, "description")
	}
	if cmd.Flags().Changed("visibility") {
		vis, err := parseVisibility(visibility)
		if err != nil {
			return nil, err
		}
		req.Visibility = vis
		paths = append(paths, "visibility")
	}
	if cmd.Flags().Changed("default-branch") {
		branch := strings.TrimPrefix(strings.TrimSpace(defaultBranch), "refs/heads/")
		if branch == "" {
			return nil, errors.New("--default-branch must not be empty")
		}
		req.DefaultBranch = branch
		paths = append(paths, "default_branch")
	}
	if cmd.Flags().Changed("merge-modes") {
		var modes []ermite.MergeMode
		for _, m := range mergeModes {
			mode, err := parseMode(m)
			if err != nil {
				return nil, err
			}
			modes = append(modes, mode)
		}
		if len(modes) == 0 {
			return nil, errors.New("--merge-modes must name at least one mode: fast-forward, squash")
		}
		req.Settings = &ermite.RepoSettings{AllowedMergeModes: modes}
		paths = append(paths, "settings.allowed_merge_modes")
	}
	if cmd.Flags().Changed("failing-checks-block") {
		if req.Settings == nil {
			req.Settings = &ermite.RepoSettings{}
		}
		req.Settings.FailingChecksBlock = failingChecksBlock
		paths = append(paths, "settings.failing_checks_block")
	}
	if len(paths) == 0 {
		return nil, errors.New("nothing to change: give at least one of --description, --visibility, --default-branch, --merge-modes, --failing-checks-block")
	}
	req.UpdateMask = &fieldmaskpb.FieldMask{Paths: paths}
	return req, nil
}

func newRepoDeleteCmd(env *cliEnv) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a repository",
		Long: "Delete a repository. Without --yes you must type the repository name on standard input to\n" +
			"confirm. The server moves the data to its trash directory instead of destroying it; there is no\n" +
			"undelete command. Needs the repo:admin scope.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		name := args[0]
		// Check the configuration before asking anything.
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		if !yes {
			if err := confirmName(cmd.InOrStdin(), cmd.ErrOrStderr(), name); err != nil {
				return err
			}
		}
		if _, err := c.Repos().DeleteRepo(env.ctx(cmd), connect.NewRequest(&ermite.DeleteRepoRequest{Repo: name})); err != nil {
			return c.Describe(err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", name)
		return err
	}
	return cmd
}

// confirmName asks the user to type name and fails when the answer differs.
func confirmName(in io.Reader, prompt io.Writer, name string) error {
	_, _ = fmt.Fprintf(prompt, "This deletes repository %q from the server.\nType its name to confirm: ", name)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		_, _ = fmt.Fprintln(prompt)
		return errors.New("aborted: no confirmation read (pass --yes to skip the question)")
	}
	if strings.TrimSpace(line) != name {
		return errors.New("aborted: the name did not match")
	}
	return nil
}
