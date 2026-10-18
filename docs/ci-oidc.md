# CI: OIDC

ermite is an OpenID Connect provider for its jobs, like GitHub. A job exchanges a short-lived ID token for cloud or Vault credentials instead of storing a long-lived secret.

```yaml
permissions:
  id-token: write
```

Works with `core.getIDToken()`, `aws-actions/configure-aws-credentials`, `hashicorp/vault-action` (`method: jwt`).

## Requirements

- A **trusted** run (see [CI trust](ci.md#trust)). Agent runs get no token.
- `id-token: write` in `permissions:`.
- `ERMITE_SECRETS_KEY` set on the server.

Otherwise the action fails as on GitHub without the permission.

## Endpoints

| | |
|---|---|
| Issuer | `<ERMITE_PUBLIC_URL>/-/oidc` |
| Discovery | `/-/oidc/.well-known/openid-configuration` |
| Keys | `/-/oidc/.well-known/jwks` |

The relying party must reach the issuer. AWS, Azure and GCP need it on the public Internet; a private server works with a relying party on the same network, such as Vault.

## Token

- RS256, valid 5 minutes, GitHub's claims (`sub`, `repository`, `ref`, `sha`, `job_workflow_ref`, `environment`, …), `runner_environment: self-hosted`.
- `sub`:
  - `repo:<owner>/<repo>:environment:<name>` for a job with an environment
  - `repo:<owner>/<repo>:pull_request` for `pull_request` and `pull_request_target`
  - `repo:<owner>/<repo>:ref:<ref>` otherwise
- Default `aud`: `<ERMITE_PUBLIC_URL>/<owner>`. `getIDToken(audience)` picks another.

## Key rotation

```sh
ermite oidc rotate-key
```

The previous key stays in the JWKS for 24 hours.

## Vault example

```sh
vault auth enable jwt
vault write auth/jwt/config \
  oidc_discovery_url="https://git.example.com/-/oidc" \
  bound_issuer="https://git.example.com/-/oidc"
vault write auth/jwt/role/deploy role_type=jwt user_claim=sub \
  bound_audiences="https://git.example.com/owner" \
  bound_claims_type=glob bound_claims='{"sub":"repo:owner/proj:ref:refs/heads/main"}' \
  policies=deploy ttl=10m
```
