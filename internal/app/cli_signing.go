// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
	"bnema.dev/ermite/internal/domain"
)

// jsonSigningKey is the stable JSON shape of `signing-key show --json`.
type jsonSigningKey struct {
	PublicKey      string `json:"public_key"`
	Fingerprint    string `json:"fingerprint"`
	CommitterName  string `json:"committer_name"`
	CommitterEmail string `json:"committer_email"`
	AllowedSigners string `json:"allowed_signers"`
}

func newSigningKeyCmd(env *cliEnv) *cobra.Command {
	key := &cobra.Command{
		Use:   "signing-key",
		Short: "Show the server's signing key and manage the keys that verify pushed commits",
		Long: "The server signs the commits it creates (squash merges) with its own SSH key, so that they show as\n" +
			"verified: `show` prints that key. Needs a server with the signing_key feature (see `ermite version`).\n\n" +
			"The server also checks the signatures of the commits you push, SSH and GPG, against the public keys\n" +
			"you trust: `trust add|list|remove` manage them. A commit shows as verified when its signature is valid,\n" +
			"made by a trusted key, and the key is trusted for the committer's email. The server's own key is\n" +
			"always trusted (builtin) and cannot be removed. Needs the commit_signatures feature (see\n" +
			"`ermite version`); adding and removing keys is for the owner.",
	}
	key.AddCommand(newSigningKeyShowCmd(env), newSigningKeyTrustCmd(env))
	return key
}

// jsonTrustedKey is the stable JSON shape of a trusted key.
type jsonTrustedKey struct {
	ID        string   `json:"id"`
	Format    string   `json:"format"`
	Title     string   `json:"title"`
	PublicKey string   `json:"public_key"`
	Emails    []string `json:"emails"`
	Builtin   bool     `json:"builtin"`
	CreatedAt *string  `json:"created_at"`
}

const jsonTrustedKeyHelp = "\n\nJSON fields of a key (--json): id (SHA256:... for ssh, the fingerprint in hex for gpg), format\n" +
	"(ssh, gpg), title, public_key, emails (array), builtin (true for the server's own key), created_at\n" +
	"(RFC 3339 UTC, null for the builtin key)."

func formatName(f ermite.SignatureFormat) string {
	switch f {
	case ermite.SignatureFormat_SIGNATURE_FORMAT_SSH:
		return "ssh"
	case ermite.SignatureFormat_SIGNATURE_FORMAT_GPG:
		return "gpg"
	case ermite.SignatureFormat_SIGNATURE_FORMAT_X509:
		return "x509"
	case ermite.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED:
	}
	return "unknown"
}

func toJSONTrustedKey(k *ermite.TrustedKey) jsonTrustedKey {
	return jsonTrustedKey{
		ID: k.GetId(), Format: formatName(k.GetFormat()), Title: k.GetTitle(), PublicKey: k.GetPublicKey(),
		Emails: append([]string{}, k.GetEmails()...), Builtin: k.GetBuiltin(), CreatedAt: jsonTime(k.GetCreatedAt()),
	}
}

// requireCommitSignatures refuses to go on with a server that does not list
// commit_signatures: it has no TrustedKeyService. A server whose features
// cannot be read is refused too.
func requireCommitSignatures(ctx context.Context, c *client.Client) error {
	res, err := c.Server().GetVersion(ctx, connect.NewRequest(&ermite.GetVersionRequest{}))
	if err != nil {
		return c.Describe(err)
	}
	if !slices.Contains(res.Msg.GetFeatures(), client.FeatureCommitSignatures) {
		return fmt.Errorf("the server does not support %s, so it cannot manage trusted keys (upgrade the server, see `ermite version`)", client.FeatureCommitSignatures)
	}
	return nil
}

func newSigningKeyTrustCmd(env *cliEnv) *cobra.Command {
	trust := &cobra.Command{
		Use:   "trust",
		Short: "Manage the keys commit signatures are verified against",
		Long: "List, add and remove the public keys the server trusts to verify the signatures of commits (SSH and\n" +
			"GPG). Each key is trusted for one to ten committer emails: a commit signed by the key but made\n" +
			"under another email shows as unverified. Needs a server with the commit_signatures feature." + jsonTrustedKeyHelp,
	}
	trust.AddCommand(newTrustListCmd(env), newTrustAddCmd(env), newTrustRemoveCmd(env))
	return trust
}

func newTrustListCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the trusted keys",
		Long: "List the trusted keys, the server's own (builtin) key first. Any authenticated token may." + "\nJSON: a trusted key; see `ermite signing-key trust --help`." +
			"\n--json prints an array of keys.",
		Args: cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		if err := requireCommitSignatures(ctx, c); err != nil {
			return err
		}
		res, err := c.TrustedKeys().ListTrustedKeys(ctx, connect.NewRequest(&ermite.ListTrustedKeysRequest{}))
		if err != nil {
			return c.Describe(err)
		}
		keys := res.Msg.GetKeys()
		if *asJSON {
			out := make([]jsonTrustedKey, 0, len(keys))
			for _, k := range keys {
				out = append(out, toJSONTrustedKey(k))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(keys) == 0 {
			noticef(cmd, "no trusted keys")
			return nil
		}
		rows := make([][]string, 0, len(keys))
		for _, k := range keys {
			builtin := "-"
			if k.GetBuiltin() {
				builtin = "yes"
			}
			rows = append(rows, []string{k.GetId(), formatName(k.GetFormat()), k.GetTitle(), strings.Join(k.GetEmails(), ","), builtin})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "FORMAT", "TITLE", "EMAILS", "BUILTIN"}, rows)
	}
	return cmd
}

func newTrustAddCmd(env *cliEnv) *cobra.Command {
	var (
		format string
		title  string
		emails []string
	)
	cmd := &cobra.Command{
		Use:   "add --format ssh|gpg --title <title> --email <email> <file|->",
		Short: "Trust a public key",
		Long: "Trust a public key for the given committer emails. The key is read from the file, or from standard\n" +
			"input with -, up to 64 KiB: an authorized_keys line for --format ssh (for example ~/.ssh/id_ed25519.pub),\n" +
			"an armored public key for --format gpg (`gpg --armor --export <fingerprint>`, exactly one key, never\n" +
			"a private key). --email is required, repeatable, 1 to 10 addresses; the title is 1 to 100 bytes.\n" +
			"Only the owner may add a key. Prints the key id on standard output, details on standard error;\n" +
			"with --json, standard output is the key object." + "\nJSON: a trusted key; see `ermite signing-key trust --help`.",
		Example: "  ermite signing-key trust add --format ssh --title laptop --email me@example.com ~/.ssh/id_ed25519.pub\n" +
			"  gpg --armor --export 0xABCD | ermite signing-key trust add --format gpg --title work --email me@example.com -",
		Args: cobra.ExactArgs(1),
	}
	cmd.Flags().StringVar(&format, "format", "", "key format: ssh or gpg")
	cmd.Flags().StringVar(&title, "title", "", "label of the key")
	cmd.Flags().StringSliceVar(&emails, "email", nil, "committer email the key may sign for, repeatable")
	_ = cmd.MarkFlagRequired("format")
	_ = cmd.MarkFlagRequired("title")
	_ = cmd.MarkFlagRequired("email")
	_ = cmd.RegisterFlagCompletionFunc("format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"ssh", "gpg"}, cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var pbFormat ermite.SignatureFormat
		switch f, err := domain.ParseSignatureFormat(format); {
		case err != nil:
			return fmt.Errorf("invalid --format %q: use ssh or gpg", format)
		case f == domain.FormatGPG:
			pbFormat = ermite.SignatureFormat_SIGNATURE_FORMAT_GPG
		default:
			pbFormat = ermite.SignatureFormat_SIGNATURE_FORMAT_SSH
		}
		key, err := readTrustedKey(cmd, args[0])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		if err := requireCommitSignatures(ctx, c); err != nil {
			return err
		}
		res, err := c.TrustedKeys().AddTrustedKey(ctx, connect.NewRequest(&ermite.AddTrustedKeyRequest{
			Format: pbFormat, Title: title, PublicKey: key, Emails: emails,
		}))
		if err != nil {
			return c.Describe(err)
		}
		k := res.Msg.GetKey()
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONTrustedKey(k))
		}
		noticef(cmd, "trusted %s key %q for %s", formatName(k.GetFormat()), clean(k.GetTitle()), clean(strings.Join(k.GetEmails(), ", ")))
		_, err = fmt.Fprintln(cmd.OutOrStdout(), clean(k.GetId()))
		return err
	}
	return cmd
}

// readTrustedKey reads a public key from a file, or from standard input for
// "-", refusing more than domain.MaxTrustedKeyBytes.
func readTrustedKey(cmd *cobra.Command, path string) (string, error) {
	var r io.Reader
	name := "standard input"
	if path == "-" {
		r = cmd.InOrStdin()
	} else {
		f, err := os.Open(path) //nolint:gosec // the user names the file
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		r, name = f, path
	}
	b, err := io.ReadAll(io.LimitReader(r, domain.MaxTrustedKeyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	if len(b) > domain.MaxTrustedKeyBytes {
		return "", fmt.Errorf("%s: the key is larger than %d KiB", name, domain.MaxTrustedKeyBytes>>10)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", errors.New(name + ": no key to read")
	}
	return key, nil
}

func newTrustRemoveCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Stop trusting a key",
		Long: "Remove a trusted key by its id (see `signing-key trust list`). Commits it signed then show as\n" +
			"unverified. The server's own (builtin) key cannot be removed. Only the owner may remove a key.",
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		if err := requireCommitSignatures(ctx, c); err != nil {
			return err
		}
		if _, err := c.TrustedKeys().RemoveTrustedKey(ctx, connect.NewRequest(&ermite.RemoveTrustedKeyRequest{Id: args[0]})); err != nil {
			return c.Describe(err)
		}
		noticef(cmd, "removed trusted key %s", clean(args[0]))
		return nil
	}
	return cmd
}

func newSigningKeyShowCmd(env *cliEnv) *cobra.Command {
	var allowedSigners, publicKey bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the public signing key of the server",
		Long: "Print the public key of the server, its fingerprint and the committer it records. The key is public:\n" +
			"no token is needed. Register it for the committer's email on Forgejo or GitHub, or save the\n" +
			"allowed-signers line to a file to verify commits locally:\n\n" +
			"  ermite signing-key show --allowed-signers >> ~/.config/git/allowed_signers\n" +
			"  git config gpg.ssh.allowedSignersFile ~/.config/git/allowed_signers\n" +
			"  git log --show-signature\n\n" +
			"--public-key prints only the authorized_keys line, --allowed-signers only the allowed-signers line\n" +
			"(\"<committer email> <key>\"), each ready to be redirected into a file.\n\n" +
			"JSON fields (--json): public_key, fingerprint (SHA256:...), committer_name, committer_email,\n" +
			"allowed_signers.",
		Args: cobra.NoArgs,
	}
	cmd.Flags().BoolVar(&publicKey, "public-key", false, "print only the key as an authorized_keys line")
	cmd.Flags().BoolVar(&allowedSigners, "allowed-signers", false, "print only the line for an allowed signers file")
	cmd.MarkFlagsMutuallyExclusive("public-key", "allowed-signers")
	asJSON := addJSONFlag(cmd)
	cmd.MarkFlagsMutuallyExclusive("json", "public-key")
	cmd.MarkFlagsMutuallyExclusive("json", "allowed-signers")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Server().GetSigningKey(env.ctx(cmd), connect.NewRequest(&ermite.GetSigningKeyRequest{}))
		if err != nil {
			return c.Describe(err)
		}
		k := res.Msg
		out := cmd.OutOrStdout()
		switch {
		case *asJSON:
			return writeJSON(out, jsonSigningKey{
				PublicKey: k.GetPublicKey(), Fingerprint: k.GetFingerprint(), CommitterName: k.GetCommitterName(),
				CommitterEmail: k.GetCommitterEmail(), AllowedSigners: k.GetAllowedSigners(),
			})
		case publicKey:
			_, err = fmt.Fprintln(out, clean(k.GetPublicKey()))
		case allowedSigners:
			_, err = fmt.Fprintln(out, clean(k.GetAllowedSigners()))
		default:
			err = fields(out, [][2]string{
				{"fingerprint", k.GetFingerprint()},
				{"committer", k.GetCommitterName() + " <" + k.GetCommitterEmail() + ">"},
				{"public key", k.GetPublicKey()},
				{"allowed signers", k.GetAllowedSigners()},
			})
		}
		return err
	}
	return cmd
}
