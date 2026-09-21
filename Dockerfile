FROM docker.io/library/golang:1.27.1-alpine3.24 AS builder
WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
COPY . .
RUN go mod tidy
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w \
      -X github.com/PrPlanIT/HASteward/src/version.Version=${VERSION} \
      -X github.com/PrPlanIT/HASteward/src/version.Commit=${COMMIT} \
      -X github.com/PrPlanIT/HASteward/src/version.BuildDate=${BUILD_DATE}" \
    -o /hasteward ./cmd/hasteward

# Fetch restic binary
FROM docker.io/library/alpine:3.24.2 AS restic
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache curl bzip2 && \
    RESTIC_VERSION=0.17.3 && \
    curl -fsSL "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_${TARGETOS:-linux}_${TARGETARCH:-amd64}.bz2" \
    | bunzip2 > /restic && chmod +x /restic
# A /tmp for the scratch stage to copy. scratch has no filesystem at all, and restic
# writes a temp pack per blob under os.TempDir() — so without this every backup and
# every escrow dies on
#   Fatal: failed to save blob ...: open /tmp/restic-temp-pack-N: no such file or directory
# It cannot be created in the final stage: scratch has no shell to RUN one.
RUN mkdir -p /image-tmp

FROM scratch
LABEL maintainer="PrPlanIT <precisionplanit@gmail.com>" \
      org.opencontainers.image.title="HASteward" \
      org.opencontainers.image.description="High Availability Steward - database cluster operator with restic-backed dedup backups" \
      org.opencontainers.image.source="https://github.com/PrPlanIT/HASteward" \
      org.opencontainers.image.url="https://hub.docker.com/r/prplanit/hasteward" \
      org.opencontainers.image.documentation="https://github.com/PrPlanIT/HASteward#readme" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.vendor="PrPlanIT"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /hasteward /hasteward
COPY --from=restic /restic /usr/bin/restic
# The mode belongs on the COPY, not on the source: this copies the CONTENTS of
# /image-tmp, and the destination is created by the builder at its own default
# (0755), so a chmod in the stage above would not survive. 1777 so the image also
# works under runAsNonRoot, which ApplyHelperHardening sets on helper pods.
COPY --from=restic --chmod=1777 /image-tmp /tmp
# restic's cache defaults to $XDG_CACHE_HOME, else $HOME/.cache. Neither is set in a
# scratch image and / is not writable, so point it inside the writable /tmp above.
ENV XDG_CACHE_HOME=/tmp/cache
ENTRYPOINT ["/hasteward"]
