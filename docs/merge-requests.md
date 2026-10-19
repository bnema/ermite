# Merge requests

A merge request proposes a branch for a target (the default branch unless set).

```sh
ermite mr create myrepo --source feature --title "Add feature"
ermite mr show myrepo 1 --diff
ermite mr merge myrepo 1 --head <commit> [--squash] [--delete-branch]
```

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

- **Fast-forward** moves the target. **Squash** writes one commit signed by the server's SSH key.
- The merge is refused when the branch moved since the reviewed `--head`, or when the target is not an ancestor of it.
- With `failing_checks_block` on (default), the merge is refused while a check of the head failed or is still running.
- `--delete-branch` deletes the source branch, unless it is the default branch or another open request uses it.
- `ermite repo edit --merge-modes` limits the modes per repository.
- Scopes: `mr:write` to open and comment, `mr:merge` to merge.

Verify squash commits with the `allowed_signers` line from `ermite signing-key show`:

```sh
git verify-commit <commit>
```
