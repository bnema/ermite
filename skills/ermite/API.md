# HTTP API

Call the API without the CLI at `<public URL>/-/api/ermite.<Service>/<Method>`. The contract is `ermite-api/proto/ermite/*.proto`; generated Connect procedure names are in `gen/ermite/ermiteconnect`.

## Unary requests

Use Connect `POST` with `Content-Type: application/json`. The body is the request message itself, with camelCase fields, not a wrapper. Responses use protobuf JSON: enums are names, 64-bit integers are strings, timestamps are RFC 3339 strings, bytes are base64, and default-valued fields may be absent. Unary errors have the shape `{"code":"unauthenticated","message":"unauthenticated"}`; check HTTP status before consuming a result.

Send `Authorization: Bearer <token>` for authentication; no header means anonymous. Invalid tokens fail authentication even on public reads, except `ServerService/GetVersion` and `GetSigningKey`, which ignore credentials.

```sh
curl -sS https://git.bnema.dev/-/api/ermite.RepoService/ListRepos \
  -H 'Content-Type: application/json' -d '{"pageSize":20}'

# Token needs owner rights or journal:read; keep it out of client-side bundles.
curl -sS "$ERMITE_URL/-/api/ermite.JournalService/ListActivity" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $ERMITE_TOKEN" \
  -d '{"since":"2026-10-01T00:00:00Z","pageSize":20}'
```

For an Astro page, call from server-side/build-time code:

```js
const response = await fetch('https://git.bnema.dev/-/api/ermite.RepoService/ListRepos', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ pageSize: 20 }),
});
if (!response.ok) throw new Error(`ermite: ${response.status}`);
const { repos = [], nextPageToken } = await response.json();
```

The server does not configure CORS for the API. Cross-origin browser fetch needs a same-origin proxy or CORS configured at your reverse proxy; server-side fetch does not.

## Read procedures

**Repo read** below means anonymous on public repositories; private repositories need owner rights or `repo:read` scoped to that repository. Unreadable repositories return `not_found`. This rule covers refs, commits, paths, merge requests, issues, releases and CI data, not just repository metadata.

For paginated procedures, pass `pageSize` and then the response's `nextPageToken` as `pageToken`, keeping filters unchanged; stop when the token is absent or empty. Check `ServerService/GetVersion.features` before relying on optional procedures or filters.

| Procedure | Key request fields (besides pagination where supported) | Access |
|---|---|---|
| `RepoService/ListRepos` | none | Repo read; filters the listing |
| `RepoService/GetRepo` | `repo` | Repo read |
| `GitService/ListRefs` | `repo`, `kinds` | Repo read |
| `GitService/ResolveRevision`, `GetCommit` | `repo`, `revision` | Repo read |
| `GitService/GetTree` | `repo`, `revision`, `path`, `includeLastCommit` | Repo read |
| `GitService/ListPaths` | `repo`, `revision` | Repo read; `git.list_paths` feature |
| `GitService/ListCommits` | `repo`, `revision`, `path` | Repo read |
| `GitService/GetBlob` (stream) | `repo`, `revision`, `path` | Repo read |
| `MergeRequestService/ListMergeRequests` | `repo`, `state` | Repo read |
| `MergeRequestService/GetMergeRequest` | `repo`, `number` | Repo read |
| `IssueService/ListIssues` | `repo`, `state` | Repo read; issues enabled |
| `IssueService/GetIssue` | `repo`, `number` | Repo read; issues enabled |
| `ReleaseService/ListReleases` | `repo` | Repo read; drafts hidden unless release writer |
| `ReleaseService/GetRelease` | `repo`, exactly one of `id`, `tag`, `latest` | Same as ListReleases |
| `ReleaseService/ListReleaseAssets` | `repo`, `releaseId` | Same as GetRelease |
| `ReleaseService/DownloadReleaseAsset` (stream) | `repo`, `assetId` or `tag: {tag, name}` or `latest: {latest: true, name}`, `offset` | Same as GetRelease |
| `RunService/ListRuns` | `repo`, `workflow`, `branch`, `status` | Repo read; filters need `ci.runs.filters` |
| `RunService/GetRun` | `repo`, `runId`, `attempt` | Repo read |
| `RunService/GetJob` | `repo`, `jobId`, `attempt` | Repo read |
| `RunService/GetJobLog` (stream) | `repo`, `jobId`, `attempt`, `offset`, `follow` | Repo read |
| `RunService/ListAnnotations` | `repo`, `runId`, `attempt`, `jobId` | Repo read; `ci.annotations` |
| `RunService/GetStepSummaries` | `repo`, `jobId`, `attempt` | Repo read; `ci.summaries` |
| `RunService/ListChecks` | `repo`, `sha` | Repo read |
| `RunService/ListWorkflows` | `repo` | Repo read; `ci.workflows` |
| `JournalService/ListActivity` | required `since`, optional `repo`, `kinds` | Owner or `journal:read`; `journal` feature |
| `ReleaseService/ListUnreleased` | optional `repo` | Owner or `journal:read`; `journal` feature |
| `EventService/Watch` (stream) | `topics`, `resumeAfter` | Repo read; `owner` topic is owner-only |
| `ServerService/GetVersion`, `GetSigningKey` | `{}` | Public |

A `Repo` carries `languages` (`name`, `bytes`, `percent`, largest first; empty until computed) and `languagesTruncated` (feature `repos.languages`).

Journal reads respect the token's repository restrictions. Write, admin and runner procedures also exist and require their corresponding scoped tokens; see [CLI.md](CLI.md) for scopes.

## Events and streaming

`EventService/Watch` is a Connect server stream, not SSE or newline-delimited JSON. Use `Content-Type: application/connect+json` and `Connect-Protocol-Version: 1`; plain `application/json` returns HTTP 415. The request is one framed JSON message. Each frame has a 5-byte prefix: one flag byte, then the payload length as a 4-byte unsigned big-endian integer, then that many payload bytes. Flag `0` is uncompressed JSON; flag `2` is the end-stream JSON (which can contain an error). Parse across arbitrary HTTP chunks, not one frame per network read.

`topics` accepts `home` (also the default when omitted) and `repo:<name>`; at most 64 topics. Anonymous streams only deliver visible public-repository events, excluding owner-only signals and private history. Event types: [CLI.md — Events](CLI.md#events); JSON uses the protobuf camelCase event fields inside `{"event":{...}}`.

```sh
python3 - <<'PY' | curl -sS -N --max-time 5 \
  https://git.bnema.dev/-/api/ermite.EventService/Watch \
  -H 'Content-Type: application/connect+json' \
  -H 'Connect-Protocol-Version: 1' --data-binary @-
import json, struct, sys
body = json.dumps({"topics": ["home"], "resumeAfter": "unknown"}).encode()
sys.stdout.buffer.write(b'\x00' + struct.pack('>I', len(body)) + body)
PY
```

This prints binary framing and JSON; the short timeout intentionally ends the watch. The unknown resume ID elicits `{"event":{"gap":{}}}`. Without `resumeAfter`, only new events arrive. Save each event's opaque `event.id`, reconnect with it in `resumeAfter`, and replay buffered events before following live updates. A `gap` means the cursor is unknown, too old, or events are lost: reload current state. Clients implement reconnection themselves; token expiry or revocation ends an authenticated stream.

Other procedures marked stream use the same framing; blob and asset chunks are base64 in JSON.
