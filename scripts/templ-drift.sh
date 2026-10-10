#!/usr/bin/env bash
# Fails when the committed *_templ.go files differ from what
# `go tool templ generate` produces from the .templ sources.
set -euo pipefail

cd "$(dirname "$0")/.."

go tool templ generate >/dev/null
if ! git diff --exit-code --stat -- '*_templ.go'; then
	echo "templ output is stale: run 'go tool templ generate' and commit the result" >&2
	exit 1
fi
if [ -n "$(git ls-files --others --exclude-standard -- '*_templ.go')" ]; then
	echo "templ generated files that are not committed:" >&2
	git ls-files --others --exclude-standard -- '*_templ.go' >&2
	exit 1
fi
