# ermite

A single-user Git forge in one Go binary. ermite hosts bare repositories, serves clone and push over HTTPS, and shows them in a read-only web interface.

Status: early development, not usable yet.

## Build

```sh
go build ./cmd/ermite
go test ./...
```

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
