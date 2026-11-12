# mediasoup benchmark image: Node media service (mediasoup + workers) and the
# Go signaling binary. One image, two compose services (media, signaling).
# Build context: repo root (see docker/compose.sfu.yaml).
#
# mediasoup-worker: we download mediasoup's prebuilt binary from GitHub ourselves
# and extract it with --no-same-owner, which also works in rootless builders.
# Build arg MS_WORKER_KERNEL picks the prebuilt:
#   auto     (default) kernel6 if the build machine runs kernel >= 6, else kernel5
#            (the rule mediasoup's own postinstall uses).
#   kernel6  linux-x64-kernel6. Built on Ubuntu 22.04 (glibc 2.34), runs on this
#            bookworm image. This is the one to ship to the SFU host (kernel 6.1).
#   kernel5  mediasoup publishes no kernel5 prebuilt since 3.31, so this compiles
#            the worker from source.
#   (kernel7 is not offered: it needs glibc 2.38, which bookworm does not have.)
#   docker buildx build --load --build-arg MS_WORKER_KERNEL=kernel6 \
#     -f docker/mediasoup/Dockerfile -t sfu-bench/mediasoup:<ver>-k6 .
# The downloaded binary must load (run without MEDIASOUP_VERSION it exits 41,
# the same check mediasoup's postinstall does). If the download fails or the
# binary doesn't load, the worker is compiled from source (needs python3-pip
# from the Debian mirror plus network for its subprojects; several minutes).

FROM docker.io/library/golang:1.26-bookworm AS signaling
WORKDIR /src
COPY mediasoup/signaling/go.mod mediasoup/signaling/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY mediasoup/signaling/ ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/ms-signaling .

FROM docker.io/library/node:22-bookworm AS media
WORKDIR /app/media
COPY mediasoup/media/package.json mediasoup/media/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm npm install --no-audit --no-fund --ignore-scripts
ARG MS_WORKER_KERNEL=auto
RUN set -eux; \
    V=$(node -p "require('./node_modules/mediasoup/package.json').version"); \
    case "$MS_WORKER_KERNEL" in \
      auto) K=kernel5; [ "$(uname -r | cut -d. -f1)" -ge 6 ] && K=kernel6 ;; \
      kernel5|kernel6) K="$MS_WORKER_KERNEL" ;; \
      *) echo "MS_WORKER_KERNEL must be auto, kernel5 or kernel6, got '$MS_WORKER_KERNEL'" >&2; exit 1 ;; \
    esac; \
    OUT=node_modules/mediasoup/worker/out/Release; mkdir -p "$OUT"; \
    worker_loads() { rc=0; "$OUT/mediasoup-worker" >/dev/null 2>&1 || rc=$?; [ "$rc" -eq 41 ]; }; \
    if curl -fsSL --retry 3 "https://github.com/versatica/mediasoup/releases/download/$V/mediasoup-worker-$V-linux-x64-$K.tgz" \
         | tar -xz --no-same-owner -C "$OUT" && worker_loads; then \
      echo "mediasoup-worker $V: prebuilt linux-x64-$K (MS_WORKER_KERNEL=$MS_WORKER_KERNEL)"; \
    else \
      echo "prebuilt linux-x64-$K unavailable or does not load, compiling mediasoup-worker $V from source"; \
      rm -rf "$OUT"; \
      apt-get update && apt-get install -y --no-install-recommends python3-pip && rm -rf /var/lib/apt/lists/*; \
      MEDIASOUP_SKIP_WORKER_PREBUILT_DOWNLOAD=1 npm rebuild mediasoup; \
      echo "mediasoup-worker $V: compiled from source"; \
    fi; \
    worker_loads
COPY mediasoup/media/tsconfig.json ./
COPY mediasoup/media/src ./src
RUN npm run build && npm prune --omit=dev \
 && ls -l node_modules/mediasoup/worker/out/Release/mediasoup-worker

FROM docker.io/mikefarah/yq:4 AS yq

FROM docker.io/library/node:22-bookworm-slim
# util-linux (taskset) and bash are part of the Debian base: no apt needed.
COPY --from=media /app/media /app/media
COPY --from=signaling /out/ms-signaling /app/ms-signaling
COPY --from=yq /usr/bin/yq /usr/local/bin/yq
COPY docker/mediasoup/run-media.sh docker/mediasoup/run-signaling.sh docker/mediasoup/healthcheck.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/run-media.sh /usr/local/bin/run-signaling.sh /usr/local/bin/healthcheck.sh \
 && node -e "console.log('mediasoup', require('/app/media/node_modules/mediasoup').version)"
WORKDIR /bench
CMD ["run-media.sh"]
