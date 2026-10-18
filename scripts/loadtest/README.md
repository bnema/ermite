# Load tests

`loadtest` replays a traffic scenario against an ermite server. It reports throughput and latency per request while holding open the live updates streams (`/-/events`) that real visitors keep.

## Quick start

```sh
scripts/loadtest/serve.sh                 # seeded server on :7418, pinned to 1 core
go run ./scripts/loadtest -scenario home  # closed-loop, 32 clients, 10 s
scripts/loadtest/serve.sh stop
```

`serve.sh` builds ermite from the current checkout and seeds five repositories (busy, bursty, steady, private, empty). It pins the server to the last `CPUS` cores (default 1) with `GOMAXPROCS` and `taskset`. A fast workstation would otherwise measure itself, not a small host. Keep the load generator off those cores: `taskset -c 0-15 go run ./scripts/loadtest …`.

## Modes

| Mode | Flags | Answers |
|---|---|---|
| Closed-loop | `-c 32` | Maximum throughput: clients send back to back. |
| Open-loop | `-rate 1000 -c 64` | Latency at a target load. Latency counts from when the request was due, so a slow server cannot hide its queue. Requests that find no free client are reported as **missed**. |

Other flags: `-d` duration, `-warmup`, `-streams` (multiplier of the scenario stream counts, `0` for none), `-token` (signed-in pages), `-var name=value`, `-json out.json` (to compare runs), `-url`.

The run fails (exit 1) on any request error, refused or dropped stream, or missed request.

Against a loopback server, each stream dials from its own `127.1.x.y` address. The server caps watches per client address (`usecase.MaxWatchersPerClient`) and in total (`usecase.MaxWatchers`).

## Scenarios

Built-in scenarios live in `scenarios/*.json`. A file path also works: `-scenario ./my.json`.

```json
{
  "name": "repo",
  "requests": [
    { "name": "summary", "path": "/{{repo}}", "weight": 4 },
    { "name": "commits", "path": "/{{repo}}/commits", "weight": 1 }
  ],
  "streams": [
    { "name": "events", "path": "/-/events?topic=repo:{{repo}}", "count": 50 }
  ]
}
```

- `weight` is the share of the requests.
- `status` is the expected status code (default 200).
- `{{name}}` is replaced by `-var name=value`.
- Every request is fetched once before the run; a wrong status stops it early.

## Reading the figures

The figures depend on the host. Compare runs made on the same machine with the same `CPUS`, and note the CPU in the report. The report header shows the load generator; the server is whatever `serve.sh` pinned.
