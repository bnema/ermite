// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/bnema/kvconf"
	"github.com/spf13/cobra"

	"bnema.dev/ermite/internal/adapters/client"
)

// notSet is what `config show` prints for a key without a value.
const notSet = "(not set)"

// configKeys are the keys of the CLI config file.
var configKeys = []string{keyURL, keyTokenCmd}

func newConfigCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and edit the config file of the remote commands",
		Long: "Configure remote commands in ~/.config/ermite/config (platform config directory on macOS\n" +
			"and Windows). ERMITE_CONFIG overrides the path; `ermite config show` prints it.\n" +
			"\n" +
			"  url       = https://git.example.com\n" +
			"  token.cmd = pass show ermite/token\n" +
			"\n" +
			"ERMITE_URL and ERMITE_TOKEN override the file. token.cmd runs through the system shell,\n" +
			"only when needed, at most once per run. It must print only the token on one line.\n" +
			"Use a secret manager (pass, gopass, op, bw, secret-tool...); never store the token itself.\n" +
			"\n" +
			"If ERMITE_URL differs from the file's url, token.cmd does not run: the call is anonymous,\n" +
			"with a notice to set ERMITE_TOKEN. `ermite version`, `ermite config` and shell completion\n" +
			"never run token.cmd; completion is anonymous unless ERMITE_TOKEN is set.\n" +
			"\n" +
			"On Unix, token.cmd is refused if another user owns the file or others can write the file\n" +
			"or its directory. `ermite config show` and `set` warn. Windows has no such check:\n" +
			"keep the file in your own profile (the default).",
	}
	cmd.AddCommand(newConfigShowCmd(env), newConfigSetCmd(env), newConfigUnsetCmd(env))
	return cmd
}

// jsonConfig is the stable JSON shape of `ermite config show --json`.
type jsonConfig struct {
	File     string  `json:"file"`
	URL      *string `json:"url" help:"null when unset"`
	TokenCmd *string `json:"token_cmd" help:"null when unset"`
}

func newConfigShowCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the config file path and its settings",
		Long: "Print the file path, url and token.cmd; absent values show as (not set).\n" +
			"The command never runs token.cmd; it warns when file permissions would block it.\n" +
			"\n" +
			jsonFieldsHelp("the settings", jsonConfig{}),
		Args: cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		f, path, err := env.readFile(cmd, true)
		if path == "" {
			p, perr := env.path()
			if perr != nil {
				return perr
			}
			path = p
		}
		if err != nil {
			// Say which file is unreadable.
			_ = fields(cmd.OutOrStdout(), [][2]string{{"file", path}})
			return err
		}
		if strings.TrimSpace(env.getenv(client.EnvURL)) != "" && f.URL != "" {
			noticef(cmd, "%s is set and takes precedence over url", client.EnvURL)
		}
		if strings.TrimSpace(env.getenv(client.EnvToken)) != "" && f.TokenCmd != "" {
			noticef(cmd, "%s is set: token.cmd is not used", client.EnvToken)
		}
		if f.TokenCmd != "" && env.fileTrust != nil {
			warnf(cmd, "%s", env.fileTrust)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), jsonConfig{File: path, URL: nilIfBlank(f.URL), TokenCmd: nilIfBlank(f.TokenCmd)})
		}
		return fields(cmd.OutOrStdout(), [][2]string{
			{"file", path},
			{keyURL, orNotSet(f.URL)},
			{keyTokenCmd, orNotSet(f.TokenCmd)},
		})
	}
	return cmd
}

func nilIfBlank(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func orNotSet(s string) string {
	if strings.TrimSpace(s) == "" {
		return notSet
	}
	return s
}

func newConfigSetCmd(env *cliEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "set <url|token.cmd> <value>",
		Short: "Set the server address or the token command in the config file",
		Long: "Set a config value, preserving comments. The command creates the file (mode 0600) if needed.\n" +
			"url must be http(s), without credentials. token.cmd prints the token (e.g. `pass show\n" +
			"ermite/token`); the command stores it as is and does not run it.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeConfigKeys,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], strings.TrimSpace(args[1])
			if err := checkConfigKey(key); err != nil {
				return err
			}
			switch key {
			case keyURL:
				u, err := client.ParseURL(value)
				if err != nil {
					return fmt.Errorf("invalid url: %w", err)
				}
				value = u.String()
			case keyTokenCmd:
				if value == "" {
					return errors.New("token.cmd is empty: use `ermite config unset token.cmd` to remove it")
				}
				if strings.ContainsAny(value, "\r\n") {
					return errors.New("token.cmd must be a single line")
				}
			}
			path, err := env.path()
			if err != nil {
				return err
			}
			if err := kvconf.SetFile(path, map[string]*string{key: &value}, 0o600); err != nil {
				return fmt.Errorf("writing %s: %w", path, err)
			}
			noticef(cmd, "%s set in %s", key, path)
			// Say now, not at the first command, that token.cmd would be refused.
			if f, _, trust, err := loadConfigFile(path); err == nil && trust != nil && f.TokenCmd != "" {
				warnf(cmd, "%s", trust)
			}
			return nil
		},
	}
}

func newConfigUnsetCmd(env *cliEnv) *cobra.Command {
	return &cobra.Command{
		Use:               "unset <url|token.cmd>",
		Short:             "Remove a setting from the config file",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeConfigKeys,
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			if err := checkConfigKey(key); err != nil {
				return err
			}
			path, err := env.path()
			if err != nil {
				return err
			}
			data, err := kvconf.ReadFile(path, kvconf.Options{})
			if errors.Is(err, fs.ErrNotExist) {
				noticef(cmd, "%s is not set: %s does not exist", key, path)
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			entries, _, err := kvconf.ParseBytes(data, kvconf.Options{})
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			if !slices.ContainsFunc(entries, func(e kvconf.Entry) bool { return e.Key == key }) {
				noticef(cmd, "%s is not set", key)
				return nil
			}
			if err := kvconf.SetFile(path, map[string]*string{key: nil}, 0o600); err != nil {
				return fmt.Errorf("writing %s: %w", path, err)
			}
			noticef(cmd, "%s removed from %s", key, path)
			return nil
		},
	}
}

func checkConfigKey(key string) error {
	for _, k := range configKeys {
		if key == k {
			return nil
		}
	}
	return fmt.Errorf("unknown key %q: the keys are %s", key, strings.Join(configKeys, " and "))
}

func completeConfigKeys(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return configKeys, cobra.ShellCompDirectiveNoFileComp
}
