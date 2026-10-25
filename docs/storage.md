# Blob storage

Release assets and CI artifact contents share one blob store. Metadata stays in
`$ERMITE_DATA_DIR/kv`; Git repositories and CI logs stay on local disk.

With no `ERMITE_S3_BUCKET`, objects live under
`$ERMITE_DATA_DIR/blobs/objects/`. Directories are private (0700), files 0600.
Back up the entire data directory while the server is stopped. With S3, back up
both the data directory and the bucket prefix consistently.

## S3-compatible storage

Create a bucket yourself, then configure ermite. All examples use explicit keys:

```sh
export ERMITE_S3_BUCKET=forge-assets
export ERMITE_S3_PREFIX=ermite/blobs
export ERMITE_S3_ACCESS_KEY_ID_FILE=/run/secrets/s3-id
export ERMITE_S3_SECRET_ACCESS_KEY_FILE=/run/secrets/s3-secret
```

Select the endpoint and signing region for your provider:

| Service | `ERMITE_S3_ENDPOINT` | `ERMITE_S3_REGION` | `ERMITE_S3_FORCE_PATH_STYLE` |
|---|---|---|---|
| AWS S3 | unset (SDK resolves from region) | bucket region, e.g. `eu-west-1` | `0` |
| Scaleway | `https://s3.fr-par.scw.cloud` | `fr-par` | `0` |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` | `0` |
| MinIO (compose network) | `http://minio:9000` | `us-east-1` | `1` |

Any S3-compatible endpoint implementing the subset below can be used. There is
no ambient AWS credential chain: `AWS_*`, shared credential files and instance
roles are not consulted. Region and both keys are required with a bucket.
Each key supports `_FILE`; direct value and file are mutually exclusive.

On startup, ermite probes the store (disk write probe; S3 `HeadBucket`, falling
back to `ListObjectsV2` with one key on 403). Failure stops startup and names
`ERMITE_S3_BUCKET`. Success logs `blob store ready`, never credential values.
The S3 probe checks reachability/read permission, not every write permission.

Grant access to objects under the configured prefix: put, get/head, delete,
list, and create/upload/complete/abort/list multipart uploads. On AWS these are
`s3:PutObject`, `s3:GetObject`, `s3:DeleteObject`,
`s3:AbortMultipartUpload`, `s3:ListMultipartUploadParts` on the object resource,
and `s3:ListBucket`, `s3:ListBucketMultipartUploads` on the bucket resource.
Constrain listing to the prefix where supported. ermite never creates buckets or
calls bucket administration APIs (policy, ACL, tagging, lifecycle, versioning).
The client uses checksums only when required, not automatic CRC32 on every PUT.

Downloads are relayed through ermite with repository authorization and Range
support: no redirects or presigned URLs. Multipart uploads use 8 MiB parts.
The daily sweep removes unreferenced blobs and unfinished uploads older than
24 hours. Release metadata with a missing blob is removed only after two sweeps.
MinIO lists unfinished uploads only for an exact object key, so the sweep does
not see them there; MinIO removes them itself (`stale_uploads_expiry`, 24 hours
by default).

## Backend changes

Legacy artifacts under `$ERMITE_DATA_DIR/ci/artifacts` remain readable until
expiry; there is no automatic copy. New artifacts and all release assets use the
currently selected backend. **Do not switch backends with live blob-backed data**:
records identify blobs, not their backend. Switching disk to S3, changing bucket
or prefix, or switching back makes earlier blob-backed contents unavailable.
Keep the selected backend stable, or arrange a complete offline copy yourself.

## Development compose

Disk (no MinIO services):

```sh
ERMITE_S3_BUCKET= docker compose up
```

MinIO provisioning must finish before the forge starts its probe:

```sh
docker compose --profile s3 up -d minio
docker compose --profile s3 run --rm minio-init
ERMITE_S3_BUCKET=ermite ERMITE_S3_ENDPOINT=http://minio:9000 \
ERMITE_S3_REGION=us-east-1 ERMITE_S3_ACCESS_KEY_ID=minioadmin \
ERMITE_S3_SECRET_ACCESS_KEY=minioadmin ERMITE_S3_FORCE_PATH_STYLE=1 \
ERMITE_S3_PREFIX=dev/ docker compose --profile s3 up ermite
```

Console: <http://localhost:9001>, development credentials `minioadmin` /
`minioadmin`. The init service creates `ermite` and `ermite-test`; only that
provisioning service has bucket creation duties. For Scaleway or another external
service, use the same environment variables with its existing bucket, endpoint,
region and keys, and `docker compose up ermite` without the profile. These are
development settings; never use the placeholder admin token or MinIO keys in
production.

## Issue history (schema 8)

Issue events are protobuf `IssueEvent` records under
`isev/<repo>/<issue:010d>/<seq:010d>`. Sequences are dense, per-issue and
1-based. Events naming a merge request are copied verbatim to
`mrev/<repo>/<mr:010d>/<issue:010d>/<seq:010d>` in the same atomic batch as
the issue update or link diff. Issue history is read in sequence order; MR
history is ordered by timestamp, then issue and sequence (at most 500 rows).
Each issue retains at most 500 events; further events are silently dropped
without preventing state or link updates. Removing a link preserves history;
deleting its repository removes both prefixes.

Schema 8 adds the resolved issue state and migrates closed issues with a
nonzero `closed_by_merge_request` to resolved, without changing timestamps,
open indexes, or creating retroactive events. The migration and schema bump
are atomic and idempotent. Like token upgrades, this uses a single batch
(16 MiB limit, typically over 10k issue records). Older builds refuse schema 8.

## Dependency runs (schema 11)

Bernard's dependency records live under `dpend/<repo>`. `run_id` links the run
that is applying the updates, or the last one that tried; `run_updates` is the
snapshot of every update that run commits. A `CIJob` of a dependency run carries
`deps` (the updates the runner applies).

Schema 11 fails the records that were applying (their job lived in memory) with
"interrupted by an upgrade: open the merge request again", cuts the error of the
failed ones to its first line and drops the old job id. The migration and the
schema bump share one batch and are idempotent. Older builds refuse schema 11.
