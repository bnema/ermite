# Git over HTTP

```sh
git clone https://git.example.com/<repo>.git              # public: no credentials
git push https://x:<token>@git.example.com/<repo>.git main
```

- The token is the password; the username is ignored. `Authorization: Bearer <token>` and `token <token>` work too.
- Use the admin token or a stored token. Keep it in a credential helper, not in the URL.
- Pushing to a new name creates a private repository.
- Repository names are lowercase `[a-z0-9._-]`.
- GitHub-shaped URLs work: `https://git.example.com/<owner>/<repo>` with `<owner>` = `ERMITE_OWNER`, `.git` optional, case-insensitive. Any other owner is a 404.
- TLS is terminated by your proxy. List its network in `ERMITE_TRUSTED_PROXIES`.
