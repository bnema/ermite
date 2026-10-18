# CI: artifacts

`actions/upload-artifact` and `actions/download-artifact` v4 and later work. ermite stores the artifacts; the runner serves them to the jobs.

```sh
ermite run artifacts <repo> <run>
ermite run download <repo> <run> [--name N]… [--dir D]
```

The run page lists them with download links.

## Rules

- A job sees only the artifacts of its own run. A re-run that uploads again replaces the earlier one.
- Never served anonymously, even for a public repository: `repo:read` to list and download, `ci:write` to delete.
- Stored under `$ERMITE_DATA_DIR/ci/artifacts`. Removed when they expire, or with their run or repository.
- An upload not finished when its job ends is dropped.

## Limits

| Setting | Default |
|---|---|
| `ERMITE_CI_ARTIFACT_MAX` (one artifact) | `5GiB` |
| `ERMITE_CI_ARTIFACT_QUOTA` (per repository) | `10GiB` |
| `ERMITE_CI_ARTIFACT_RETENTION` (max `retention-days`) | `90d` |

Also: 32 unfinished uploads per job, 1000 artifacts per run. `ERMITE_CI_ARTIFACTS=0` turns artifacts off.

## Not supported

- Artifacts v3 (`@actions/artifact` v1).
- The REST endpoints `/actions/runs/{id}/artifacts` and `/actions/artifacts/{id}/zip`: `download-artifact` with `run-id` and `github-token` gets a 404.
