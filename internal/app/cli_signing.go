// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
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
		Short: "Show the key that signs the commits the server creates",
		Long: "The server signs the commits it creates (squash merges) with its own SSH key, so that they show as\n" +
			"verified. Needs a server with the signing_key feature (see `ermite version`).",
	}
	key.AddCommand(newSigningKeyShowCmd(env))
	return key
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
