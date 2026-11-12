# LiveKit benchmark image: the open-source livekit-server binary (statically
# linked, from the official image) on Debian slim, plus bash/taskset (util-linux)
# and yq to read bench.yaml. No package installs.
# Keep the version in step with livekit.version in bench.yaml (used by the native
# livekit/get-binary.sh), so native and docker runs use the same release.
# Build context: repo root (see docker/compose.sfu.yaml).
ARG LIVEKIT_IMAGE=docker.io/livekit/livekit-server:v1.13.11
FROM ${LIVEKIT_IMAGE} AS upstream
FROM docker.io/mikefarah/yq:4 AS yq

FROM docker.io/library/debian:bookworm-slim
COPY --from=upstream /livekit-server /usr/local/bin/livekit-server
COPY --from=yq /usr/bin/yq /usr/local/bin/yq
COPY docker/livekit/run.sh /usr/local/bin/run-livekit
RUN chmod +x /usr/local/bin/run-livekit && /usr/local/bin/livekit-server --version
WORKDIR /bench
ENTRYPOINT ["/usr/local/bin/run-livekit"]
