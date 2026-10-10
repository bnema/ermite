#!/bin/sh
# Check the protobuf schema against the base branch with buf breaking.
# Usage: scripts/buf-breaking.sh [base-ref]   (default: origin/main)
#
# The check is skipped while the base has no proto/ directory (the change that
# introduces the schema has nothing to be compared with).
set -eu

base="${1:-origin/main}"

if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
	echo "buf breaking: base $base not found, fetch it first" >&2
	exit 1
fi
if ! git cat-file -e "$base:proto" 2>/dev/null; then
	echo "buf breaking: $base has no proto/ directory, nothing to compare"
	exit 0
fi
GOWORK=off go tool buf breaking --against ".git#ref=$base"
