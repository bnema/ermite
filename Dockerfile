# syntax=docker/dockerfile:1

# Release image: a static ermite binary on alpine with git and git-http-backend (ermite shells out to git >= 2.45).
#
#   docker build --build-arg VERSION="$(git describe --tags --always --dirty)" -t ermite .
#
# The modules bnema.dev/ermite-* are private and resolve through git.bnema.dev, which is
# reachable from the NetBird network only. The build container uses the host's DNS and
# routes, so on a NetBird host it needs nothing more. Behind a different route (CI), set
#   --build-arg GIT_REWRITE_BASE=http://git.bnema.dev:8088/ --network host
# to fetch the modules over that URL instead of https://git.bnema.dev/.
#
# Base images are pinned to an Alpine minor release; bump them on purpose.

FROM golang:1.27-alpine3.24 AS build
RUN apk add --no-cache git
ENV CGO_ENABLED=0 GOWORK=off GOTOOLCHAIN=local GOPRIVATE=bnema.dev/ermite*
ARG GIT_REWRITE_BASE=
RUN if [ -n "$GIT_REWRITE_BASE" ]; then \
      git config --global url."$GIT_REWRITE_BASE".insteadOf https://git.bnema.dev/; \
    fi
WORKDIR /src
COPY . .
ARG VERSION=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.stamped=${VERSION}" \
      -o /out/ermite ./cmd/ermite

FROM alpine:3.24
# git-daemon provides git-http-backend on alpine. tini runs as PID 1: it forwards signals
# to ermite and reaps the git processes that detach from pushes (auto maintenance).
RUN apk add --no-cache git git-daemon ca-certificates tini \
 && adduser -D -u 10001 -h /home/ermite ermite \
 && mkdir /data \
 && chown ermite:ermite /data
COPY --from=build /out/ermite /usr/local/bin/ermite
ENV ERMITE_DATA_DIR=/data ERMITE_LOG_FORMAT=json
USER ermite
WORKDIR /data
VOLUME /data
EXPOSE 7341
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/ermite"]
CMD ["serve"]
