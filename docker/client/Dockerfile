# Load generator image: the loadgen binary (agent / ctl / report / compare / run),
# plus curl (scripts/run-scenario.sh), a static ffmpeg with libvpx
# (client/scripts/make-media.sh) and yq.
# Build context: repo root (see docker/compose.gen.yaml).
# apt (curl only) needs a Debian mirror; configure it in Docker if hosts have no internet.

FROM docker.io/library/golang:1.26-bookworm AS build
WORKDIR /src
COPY client/go.mod client/go.sum ./
COPY client/third_party/ ./third_party/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY client/ ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    H=$(bash scripts/source-hash.sh) \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.sourceHash=$H" -o /out/loadgen .

FROM docker.io/mikefarah/yq:4 AS yq
FROM docker.io/mwader/static-ffmpeg:7.1 AS ffmpeg

FROM docker.io/library/debian:bookworm-slim
# APT::Sandbox::User=root: apt can't drop privileges in rootless builders (podman/buildkit rootless).
RUN echo 'APT::Sandbox::User "root";' > /etc/apt/apt.conf.d/99sandbox \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/loadgen /usr/local/bin/loadgen
COPY --from=yq /usr/bin/yq /usr/local/bin/yq
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
COPY docker/client/run-agent.sh /usr/local/bin/run-agent.sh
RUN chmod +x /usr/local/bin/run-agent.sh && ffmpeg -hide_banner -encoders 2>/dev/null | grep -q libvpx
# The repo is mounted here; loadgen finds /bench/bench.yaml and /bench/client/media.
WORKDIR /bench
ENTRYPOINT ["loadgen"]
