# Commit signatures

ermite checks commit signatures on the commit list, the commit page and the commits of a merge request, and shows a badge with the protocol and the result. The API returns the same result in `Commit.verification` from `ListCommits`, `GetCommit` and `GetMergeRequest`.

## Formats

| Format | Git setting | Checked |
|---|---|---|
| SSH | `gpg.format=ssh` | yes |
| OpenPGP | `gpg.format=openpgp` (default) | yes |
| X.509 | `gpg.format=x509` | no, shown as unverified |

Signatures are checked in the server, without `gpg` or `ssh-keygen`.

## States

| Badge | Meaning |
|---|---|
| `SSH Verified` / `GPG Verified` | valid signature, trusted key, committer email listed for that key |
| `Unverified` | unknown key, email not listed for the key, expired or revoked OpenPGP key, or X.509; the tooltip says which |
| `Bad signature` | the signature is malformed or does not match the commit |
| none (list) / `unsigned` (commit page) | no signature |

An OpenPGP key's expiry is judged when the signature was made; a revoked key is unverified for all its commits. OpenPGP signatures over SHA-1 count as bad signatures.

## Trusted keys

Only the owner (the admin token, or a token with every scope, no repository list and no branch prefix) changes the list. Any authenticated caller can read it.

```sh
ermite signing-key trust add --format ssh --title laptop --email me@example.com ~/.ssh/id_ed25519.pub
gpg --armor --export me@example.com | ermite signing-key trust add --format gpg --title gpg --email me@example.com -
ermite signing-key trust list
ermite signing-key trust remove <id>
```

- A key may sign for 1 to 10 emails; up to 100 keys.
- The id is the fingerprint: `SHA256:…` for SSH, the primary key fingerprint for OpenPGP.
- The server's own key (`ermite signing-key show`), which signs squash merges, is always trusted for the committer email and cannot be removed.
- Private keys are refused.
