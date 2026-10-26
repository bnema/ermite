// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
)

// jsonOIDCRotation is the stable JSON shape of `oidc rotate-key --json`.
type jsonOIDCRotation struct {
	Issuer       string  `json:"issuer"`
	Kid          string  `json:"kid"`
	RetiredKid   *string `json:"retired_kid"`
	RetiredUntil *string `json:"retired_until"`
}

func newOIDCCmd(env *cliEnv) *cobra.Command {
	oidc := &cobra.Command{
		Use:   "oidc",
		Short: "Manage the OIDC provider of CI jobs",
		Long: "Manage OIDC ID tokens for trusted CI runs with `permissions: id-token: write`, as on GitHub.\n" +
			"A relying party (Vault, a cloud) trusting issuer <public URL>/-/oidc exchanges them for credentials.\n" +
			"The server signs tokens with its own key, sealed with ERMITE_SECRETS_KEY; without it, no tokens\n" +
			"are issued. Requires ci.oidc (see `ermite version`).",
	}
	oidc.AddCommand(newOIDCRotateKeyCmd(env))
	return oidc
}

func newOIDCRotateKeyCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate-key",
		Short: "Replace the key that signs the ID tokens",
		Long: "Generate a new signing key for the ID tokens. The new key signs at once; the previous one stays\n" +
			"in the JWKS (<issuer>/.well-known/jwks) for 24 hours, so that the tokens it signed verify until they\n" +
			"expire. Needs owner rights: the admin token, or a token with every scope, no repository list\n" +
			"and no branch prefixes.\n\n" +
			"JSON fields (--json): issuer, kid, retired_kid and retired_until (RFC 3339 UTC; both null when\n" +
			"there was no previous key).",
		Args: cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Server().RotateOIDCKey(env.ctx(cmd), connect.NewRequest(&ermite.RotateOIDCKeyRequest{}))
		if err != nil {
			return c.Describe(err)
		}
		r := res.Msg
		out := cmd.OutOrStdout()
		if *asJSON {
			j := jsonOIDCRotation{Issuer: r.GetIssuer(), Kid: r.GetKid(), RetiredUntil: jsonTime(r.GetRetiredUntil())}
			if k := r.GetRetiredKid(); k != "" {
				j.RetiredKid = &k
			}
			return writeJSON(out, j)
		}
		pairs := [][2]string{{"issuer", r.GetIssuer()}, {"kid", r.GetKid()}}
		if r.GetRetiredKid() != "" {
			pairs = append(pairs, [2]string{"retired kid", r.GetRetiredKid()}, [2]string{"published until", shortTime(r.GetRetiredUntil(), "")})
		}
		return fields(out, pairs)
	}
	return cmd
}
