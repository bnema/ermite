# Web sign-in

Visitors read public repositories. The owner signs in to see private ones, review and merge. A session lasts 12 hours.

## Two ways in

- **One-time link**: `ermite web login` (admin token or a token with every scope) prints a link valid once, for 60 seconds. Use it the first time.
- **Passkey**: press "Sign in with a passkey" on `/-/login`. Add one from `/-/passkeys` while signed in. Up to 20.

The cookie is `Secure`: plain `http` works only on localhost.

## Passkey requirements

`ERMITE_PUBLIC_URL` must be `https` (or `http://localhost`) with a **host name, not an IP**. A passkey only works on the name it was registered for. On `http://127.0.0.1:…` passkeys are off (the log says `passkeys disabled`).

## Manage sessions

```sh
ermite web passkey list
ermite web passkey remove <id>
ermite web logout --all
```

Removing a passkey, `logout --all`, or revoking a token with every scope signs out every device. If you lose every device, `ermite web login` still works.
