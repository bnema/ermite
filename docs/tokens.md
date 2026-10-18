# Tokens

```sh
ermite token create <label> [--preset …] [--scope …] [--repo …] [--branch-prefix …] [--expires 30d]
ermite token list
ermite token revoke <id>
```

The secret is printed once. Presets and `--scope` add up. The full scope list is in [`skills/ermite/CLI.md`](../skills/ermite/CLI.md).

| Preset | For |
|---|---|
| `cli` | every scope, the owner's daily use |
| `agent` | `repo:write`, `mr:write` |
| `ci` | `repo:read` |
| `runner` | `runner:register`: a runner host. It can read the code and secrets of every repository, so give it only to runners you operate |

## Branch-limited tokens

For an AI agent that should propose changes, not publish them:

```sh
ermite token create claude --preset agent --scope repo:read --repo myrepo --branch-prefix agent/claude/
```

`--branch-prefix` is repeatable (up to 8), each ending with `/`.

The token **can**:

- push, force-push and delete branches under its prefixes;
- open, comment on and close merge requests;
- with `mr:merge`, merge into a branch under its prefixes;
- read CI runs, logs and checks; with `ci:write`, cancel and re-run runs of its own branches;
- create tokens whose prefixes lie within its own.

The token **cannot**:

- push any other ref (`main`, other branches, tags, notes). The whole push is refused and the message names the ref;
- create, delete or configure a repository;
- open web sessions or get owner rights, even with every scope;
- manage runners, even with `runner:register`;
- get trusted CI runs (no secrets). See [CI trust](ci.md#trust).

`ermite token list` shows a `branches` column (`all` when unlimited); `--json` has `branch_prefixes`.
