// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// `ermite secret`, `ermite variable` and `ermite environment`: the secrets,
// variables and environments CI jobs receive (ADR 0019), with the flags of
// `gh secret` and `gh variable`.

// settingsFlags are the scope flags of secret and variable commands.
type settingsFlags struct {
	repo     string
	env      string
	instance bool
}

func (f *settingsFlags) add(cmd *cobra.Command, env *cliEnv, what string) {
	cmd.Flags().StringVarP(&f.repo, "repo", "R", "", "the repository whose "+what+" to use")
	cmd.Flags().StringVarP(&f.env, "env", "e", "", "the environment of --repo (default: the repository's own)")
	cmd.Flags().BoolVar(&f.instance, "instance", false, "the "+what+" of the whole server (every repository)")
	cmd.MarkFlagsMutuallyExclusive("instance", "repo")
	cmd.MarkFlagsMutuallyExclusive("instance", "env")
	_ = cmd.RegisterFlagCompletionFunc("repo", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return repoNames(env, c, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
}

// scope builds the scope of the request and the words that name it in output.
func (f *settingsFlags) scope() (*ermite.SettingsScope, string, error) {
	switch {
	case f.instance:
		return &ermite.SettingsScope{Where: &ermite.SettingsScope_Instance{Instance: true}}, "the instance", nil
	case f.repo == "":
		return nil, "", errors.New("choose where it lives: --repo <repo> (with --env <environment> for an environment), or --instance")
	case f.env != "":
		return &ermite.SettingsScope{Where: &ermite.SettingsScope_Environment{Environment: &ermite.EnvironmentRef{Repo: f.repo, Name: f.env}}},
			"environment " + f.env + " of " + f.repo, nil
	}
	return &ermite.SettingsScope{Where: &ermite.SettingsScope_Repo{Repo: f.repo}}, f.repo, nil
}

func (f *settingsFlags) check() error {
	if f.env != "" && f.repo == "" {
		return errors.New("--env needs --repo: an environment belongs to a repository")
	}
	return nil
}

// --- values ---

// readValue returns the value of a secret or variable: --body, else standard
// input. A terminal is asked for it, without echo for a secret.
func readValue(cmd *cobra.Command, body string, bodySet, secret bool, name string) (string, error) {
	if bodySet {
		return body, nil
	}
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Paste the value of %s: ", name)
		var raw []byte
		var err error
		if secret {
			raw, err = term.ReadPassword(int(f.Fd()))
			_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		} else {
			var line string
			line, err = bufio.NewReader(f).ReadString('\n')
			raw = []byte(strings.TrimRight(line, "\r\n"))
			if errors.Is(err, io.EOF) && line != "" {
				err = nil
			}
		}
		if err != nil {
			return "", fmt.Errorf("read the value: %w", err)
		}
		return string(raw), nil
	}
	// A pipe: everything, minus the line break that ends it (like gh). The read
	// stops one byte past the cap, so input that exhausts it is refused and
	// never silently cut.
	const slack = 1024 // room for line breaks around a value of the largest size
	raw, err := io.ReadAll(io.LimitReader(in, domain.MaxSettingValueBytes+slack+1))
	if err != nil {
		return "", fmt.Errorf("read the value from standard input: %w", err)
	}
	if len(raw) > domain.MaxSettingValueBytes+slack {
		return "", fmt.Errorf("the value on standard input is larger than %d bytes (48 KiB)", domain.MaxSettingValueBytes)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// parseEnvFile reads KEY=VALUE lines: blank lines and # comments are skipped,
// "export " is allowed, and a value may be in matching single quotes (literal)
// or double quotes (with \n, \r, \t, \" and \\ escapes). A value is one line.
func parseEnvFile(r io.Reader) ([][2]string, error) {
	var out [][2]string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), domain.MaxSettingValueBytes*2)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("line %d: expected NAME=value", n)
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
		}
		out = append(out, [2]string{k, v})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read the file: %w", err)
	}
	return out, nil
}

// readPairs reads what `set` was asked to set: the NAME argument with its value
// (--body, standard input or a prompt), or the NAME=value lines of --env-file.
// noun is "secret" or "variable"; secret hides the prompt's echo.
func readPairs(cmd *cobra.Command, args []string, body, envFile, noun string, secret bool) ([][2]string, error) {
	switch {
	case envFile != "" && len(args) > 0:
		return nil, errors.New("give either a NAME or --env-file, not both")
	case envFile != "":
		pairs, err := openEnvFile(cmd, envFile)
		if err != nil {
			return nil, err
		}
		if len(pairs) == 0 {
			return nil, fmt.Errorf("%s holds no NAME=value line", envFile)
		}
		return pairs, nil
	case len(args) == 0:
		return nil, fmt.Errorf("give the NAME of the %s, or --env-file", noun)
	}
	value, err := readValue(cmd, body, cmd.Flags().Changed("body"), secret, args[0])
	if err != nil {
		return nil, err
	}
	return [][2]string{{args[0], value}}, nil
}

// openEnvFile reads the --env-file; "-" is standard input.
func openEnvFile(cmd *cobra.Command, path string) ([][2]string, error) {
	if path == "-" {
		return parseEnvFile(cmd.InOrStdin())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the user names the file
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseEnvFile(bytes.NewReader(raw))
}

// --- secret ---

func newSecretCmd(env *cliEnv) *cobra.Command {
	secret := &cobra.Command{
		Use:   "secret",
		Short: "Manage the secrets CI jobs receive",
		Long: "Set, list and delete the secrets that workflows read as ${{ secrets.NAME }}, like `gh secret`.\n" +
			"A secret lives in a repository, in one of its environments (--env) or on the whole instance\n" +
			"(--instance); the environment wins over the repository, and the repository over the instance.\n" +
			"Values are write-only: no command prints one. Names are letters, digits and underscores, do not\n" +
			"start with a digit or GITHUB_, and are stored in upper case; a value is up to 48 KiB.\n\n" +
			"Needs the secrets:write scope (on a token limited to some repositories, only for them; --instance\n" +
			"needs a token that is not). A token limited to branch prefixes never manages secrets. Runs started\n" +
			"by such a token do not receive secrets either. The server needs ERMITE_SECRETS_KEY to store them.",
	}
	secret.AddCommand(newSecretSetCmd(env), newSecretListCmd(env), newSecretDeleteCmd(env))
	return secret
}

func newSecretSetCmd(env *cliEnv) *cobra.Command {
	var (
		f       settingsFlags
		body    string
		envFile string
	)
	cmd := &cobra.Command{
		Use:   "set [NAME]",
		Short: "Create or update a secret",
		Long: "Create or update a secret. The value comes from --body, from standard input (a pipe or a\n" +
			"redirection) or, on a terminal, from a prompt that does not echo it. A trailing line break of\n" +
			"standard input is dropped. --env-file sets several secrets at once from a file of NAME=value lines\n" +
			"(\"-\" reads standard input); NAME is then not given.\n\n" +
			"  ermite secret set AUR_KEY --repo dumber < key.pem\n" +
			"  ermite secret set CODECOV_TOKEN --repo gordon --body \"$TOKEN\"\n" +
			"  ermite secret set REGISTRY_PASSWORD --repo app --env production\n" +
			"  ermite secret set --env-file .env --instance",
		Args: cobra.MaximumNArgs(1),
	}
	f.add(cmd, env, "secret")
	cmd.Flags().StringVarP(&body, "body", "b", "", "the value (visible in the process list: prefer standard input)")
	cmd.Flags().StringVarP(&envFile, "env-file", "f", "", "set the secrets of a file of NAME=value lines (\"-\" is standard input)")
	cmd.MarkFlagsMutuallyExclusive("body", "env-file")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := f.check(); err != nil {
			return err
		}
		scope, where, err := f.scope()
		if err != nil {
			return err
		}
		pairs, err := readPairs(cmd, args, body, envFile, "secret", true)
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		var errs []error
		for _, p := range pairs {
			res, err := c.Secrets().SetSecret(env.ctx(cmd), connect.NewRequest(&ermite.SetSecretRequest{Scope: scope, Name: p[0], Value: p[1]}))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", clean(p[0]), c.Describe(err)))
				continue
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "set secret %s for %s\n", clean(res.Msg.GetSecret().GetName()), clean(where)); err != nil {
				return err
			}
		}
		return errors.Join(errs...)
	}
	return cmd
}

type jsonSecret struct {
	Name      string  `json:"name"`
	UpdatedAt *string `json:"updated_at"`
}

func newSecretListCmd(env *cliEnv) *cobra.Command {
	var f settingsFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the names of the secrets",
		Long: "List the secrets of one scope with their last update. Only the secrets set in that scope are\n" +
			"listed, not the ones it inherits. Values are never shown.\n\n" +
			"JSON fields of a secret (--json): name, updated_at (RFC 3339 UTC, null when unset).",
		Args: cobra.NoArgs,
	}
	f.add(cmd, env, "secrets")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := f.check(); err != nil {
			return err
		}
		scope, _, err := f.scope()
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		items, err := collect(ctx, func(token string) ([]*ermite.Secret, string, error) {
			res, err := c.Secrets().ListSecrets(ctx, connect.NewRequest(&ermite.ListSecretsRequest{Scope: scope, PageToken: token}))
			if err != nil {
				return nil, "", c.Describe(err)
			}
			return res.Msg.GetSecrets(), res.Msg.GetNextPageToken(), nil
		})
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonSecret, 0, len(items))
			for _, s := range items {
				out = append(out, jsonSecret{Name: s.GetName(), UpdatedAt: jsonTime(s.GetUpdatedAt())})
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(items) == 0 {
			noticef(cmd, "no secrets")
			return nil
		}
		rows := make([][]string, 0, len(items))
		for _, s := range items {
			rows = append(rows, []string{s.GetName(), shortTime(s.GetUpdatedAt(), "")})
		}
		return table(cmd.OutOrStdout(), []string{"NAME", "UPDATED"}, rows)
	}
	return cmd
}

func newSecretDeleteCmd(env *cliEnv) *cobra.Command {
	var f settingsFlags
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a secret",
		Args:  cobra.ExactArgs(1),
	}
	f.add(cmd, env, "secret")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := f.check(); err != nil {
			return err
		}
		scope, where, err := f.scope()
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		if _, err := c.Secrets().DeleteSecret(env.ctx(cmd), connect.NewRequest(&ermite.DeleteSecretRequest{Scope: scope, Name: args[0]})); err != nil {
			return c.Describe(err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted secret %s from %s\n", clean(strings.ToUpper(args[0])), clean(where))
		return err
	}
	return cmd
}

// --- variable ---

func newVariableCmd(env *cliEnv) *cobra.Command {
	v := &cobra.Command{
		Use:   "variable",
		Short: "Manage the variables CI jobs receive",
		Long: "Set, get, list and delete the configuration variables that workflows read as ${{ vars.NAME }},\n" +
			"like `gh variable`. Scopes, names, precedence and access are those of `ermite secret`; unlike a\n" +
			"secret, a variable can be read back and may appear in logs.",
	}
	v.AddCommand(newVariableSetCmd(env), newVariableGetCmd(env), newVariableListCmd(env), newVariableDeleteCmd(env))
	return v
}

func newVariableSetCmd(env *cliEnv) *cobra.Command {
	var (
		f       settingsFlags
		body    string
		envFile string
	)
	cmd := &cobra.Command{
		Use:   "set [NAME]",
		Short: "Create or update a variable",
		Long: "Create or update a variable. The value comes from --body, from standard input or, on a terminal,\n" +
			"from a prompt. --env-file sets several from a file of NAME=value lines, as `ermite secret set`.",
		Args: cobra.MaximumNArgs(1),
	}
	f.add(cmd, env, "variable")
	cmd.Flags().StringVarP(&body, "body", "b", "", "the value")
	cmd.Flags().StringVarP(&envFile, "env-file", "f", "", "set the variables of a file of NAME=value lines (\"-\" is standard input)")
	cmd.MarkFlagsMutuallyExclusive("body", "env-file")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := f.check(); err != nil {
			return err
		}
		scope, where, err := f.scope()
		if err != nil {
			return err
		}
		pairs, err := readPairs(cmd, args, body, envFile, "variable", false)
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		var errs []error
		for _, p := range pairs {
			res, err := c.Variables().SetVariable(env.ctx(cmd), connect.NewRequest(&ermite.SetVariableRequest{Scope: scope, Name: p[0], Value: p[1]}))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", clean(p[0]), c.Describe(err)))
				continue
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "set variable %s for %s\n", clean(res.Msg.GetVariable().GetName()), clean(where)); err != nil {
				return err
			}
		}
		return errors.Join(errs...)
	}
	return cmd
}

type jsonVariable struct {
	Name      string  `json:"name"`
	Value     string  `json:"value"`
	UpdatedAt *string `json:"updated_at"`
}

func variableJSON(v *ermite.Variable) jsonVariable {
	return jsonVariable{Name: v.GetName(), Value: v.GetValue(), UpdatedAt: jsonTime(v.GetUpdatedAt())}
}

func newVariableGetCmd(env *cliEnv) *cobra.Command {
	var f settingsFlags
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Print the value of a variable",
		Long: "Print the value of a variable of one scope, and nothing else, so that it can be used in a script.\n\n" +
			"JSON fields (--json): name, value, updated_at (RFC 3339 UTC, null when unset).",
		Args: cobra.ExactArgs(1),
	}
	f.add(cmd, env, "variable")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := f.check(); err != nil {
			return err
		}
		scope, _, err := f.scope()
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Variables().GetVariable(env.ctx(cmd), connect.NewRequest(&ermite.GetVariableRequest{Scope: scope, Name: args[0]}))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), variableJSON(res.Msg.GetVariable()))
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), res.Msg.GetVariable().GetValue())
		return err
	}
	return cmd
}

func newVariableListCmd(env *cliEnv) *cobra.Command {
	var f settingsFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the variables with their values",
		Long: "List the variables of one scope with their values and last update; the ones the scope inherits are\n" +
			"not listed.\n\nJSON fields of a variable (--json): name, value, updated_at (RFC 3339 UTC, null when unset).",
		Args: cobra.NoArgs,
	}
	f.add(cmd, env, "variables")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := f.check(); err != nil {
			return err
		}
		scope, _, err := f.scope()
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		items, err := collect(ctx, func(token string) ([]*ermite.Variable, string, error) {
			res, err := c.Variables().ListVariables(ctx, connect.NewRequest(&ermite.ListVariablesRequest{Scope: scope, PageToken: token}))
			if err != nil {
				return nil, "", c.Describe(err)
			}
			return res.Msg.GetVariables(), res.Msg.GetNextPageToken(), nil
		})
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonVariable, 0, len(items))
			for _, v := range items {
				out = append(out, variableJSON(v))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(items) == 0 {
			noticef(cmd, "no variables")
			return nil
		}
		rows := make([][]string, 0, len(items))
		for _, v := range items {
			rows = append(rows, []string{v.GetName(), v.GetValue(), shortTime(v.GetUpdatedAt(), "")})
		}
		return table(cmd.OutOrStdout(), []string{"NAME", "VALUE", "UPDATED"}, rows)
	}
	return cmd
}

func newVariableDeleteCmd(env *cliEnv) *cobra.Command {
	var f settingsFlags
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a variable",
		Args:  cobra.ExactArgs(1),
	}
	f.add(cmd, env, "variable")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := f.check(); err != nil {
			return err
		}
		scope, where, err := f.scope()
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		if _, err := c.Variables().DeleteVariable(env.ctx(cmd), connect.NewRequest(&ermite.DeleteVariableRequest{Scope: scope, Name: args[0]})); err != nil {
			return c.Describe(err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted variable %s from %s\n", clean(strings.ToUpper(args[0])), clean(where))
		return err
	}
	return cmd
}

// --- environment ---

type jsonEnvironment struct {
	Repo      string  `json:"repo"`
	Name      string  `json:"name"`
	CreatedAt *string `json:"created_at"`
}

func newEnvironmentCmd(env *cliEnv) *cobra.Command {
	e := &cobra.Command{
		Use:   "environment",
		Short: "Manage the environments of a repository",
		Long: "Create, list and delete the environments of a repository, to which secrets and variables are attached\n" +
			"(`ermite secret set --env`). A job that names one with `environment:` creates it on first use. An\n" +
			"environment has no protection rules and no deployments. Access is that of `ermite secret`.",
	}
	e.AddCommand(newEnvironmentCreateCmd(env), newEnvironmentListCmd(env), newEnvironmentDeleteCmd(env))
	return e
}

func addRepoFlag(cmd *cobra.Command, env *cliEnv, repo *string) {
	cmd.Flags().StringVarP(repo, "repo", "R", "", "the repository")
	_ = cmd.MarkFlagRequired("repo")
	_ = cmd.RegisterFlagCompletionFunc("repo", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return repoNames(env, c, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
}

func newEnvironmentCreateCmd(env *cliEnv) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "create NAME --repo REPO",
		Short: "Create an environment",
		Long:  "Create an environment. One that exists already (in any case) is left as it is.",
		Args:  cobra.ExactArgs(1),
	}
	addRepoFlag(cmd, env, &repo)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Environments().CreateEnvironment(env.ctx(cmd), connect.NewRequest(&ermite.CreateEnvironmentRequest{Repo: repo, Name: args[0]}))
		if err != nil {
			return c.Describe(err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "environment %s of %s\n", clean(res.Msg.GetEnvironment().GetName()), clean(repo))
		return err
	}
	return cmd
}

func newEnvironmentListCmd(env *cliEnv) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "list --repo REPO",
		Short: "List the environments of a repository",
		Long:  "List the environments of a repository ordered by name.\n\nJSON fields of an environment (--json): repo, name, created_at (RFC 3339 UTC, null when unset).",
		Args:  cobra.NoArgs,
	}
	addRepoFlag(cmd, env, &repo)
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		items, err := collect(ctx, func(token string) ([]*ermite.Environment, string, error) {
			res, err := c.Environments().ListEnvironments(ctx, connect.NewRequest(&ermite.ListEnvironmentsRequest{Repo: repo, PageToken: token}))
			if err != nil {
				return nil, "", c.Describe(err)
			}
			return res.Msg.GetEnvironments(), res.Msg.GetNextPageToken(), nil
		})
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonEnvironment, 0, len(items))
			for _, e := range items {
				out = append(out, jsonEnvironment{Repo: e.GetRepo(), Name: e.GetName(), CreatedAt: jsonTime(e.GetCreatedAt())})
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(items) == 0 {
			noticef(cmd, "no environments")
			return nil
		}
		rows := make([][]string, 0, len(items))
		for _, e := range items {
			rows = append(rows, []string{e.GetName(), shortTime(e.GetCreatedAt(), "")})
		}
		return table(cmd.OutOrStdout(), []string{"NAME", "CREATED"}, rows)
	}
	return cmd
}

func newEnvironmentDeleteCmd(env *cliEnv) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "delete NAME --repo REPO",
		Short: "Delete an environment with its secrets and variables",
		Args:  cobra.ExactArgs(1),
	}
	addRepoFlag(cmd, env, &repo)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		if _, err := c.Environments().DeleteEnvironment(env.ctx(cmd), connect.NewRequest(&ermite.DeleteEnvironmentRequest{Repo: repo, Name: args[0]})); err != nil {
			return c.Describe(err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted environment %s of %s\n", clean(args[0]), clean(repo))
		return err
	}
	return cmd
}
