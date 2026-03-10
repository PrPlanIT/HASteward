FROM docker.io/library/golang:1.26.1-alpine3.23 AS builder
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
      -X gitlab.prplanit.com/precisionplanit/hasteward/src/version.Version=${VERSION} \
      -X gitlab.prplanit.com/precisionplanit/hasteward/src/version.Commit=${COMMIT} \
      -X gitlab.prplanit.com/precisionplanit/hasteward/src/version.BuildDate=${BUILD_DATE}" \
    -o /hasteward .

# Fetch restic binary
FROM docker.io/library/alpine:3.23.3 AS restic
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache curl bzip2 && \
    RESTIC_VERSION=0.17.3 && \
    curl -fsSL "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_${TARGETOS:-linux}_${TARGETARCH:-amd64}.bz2" \
    | bunzip2 > /restic && chmod +x /restic

FROM scratch
LABEL maintainer="SoFMeRight <sofmeright@gmail.com>" \
      org.opencontainers.image.title="HASteward" \
      org.opencontainers.image.description="High Availability Steward - database cluster operator with restic-backed dedup backups" \
      org.opencontainers.image.source="https://gitlab.prplanit.com/precisionplanit/hasteward"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /hasteward /hasteward
COPY --from=restic /restic /usr/bin/restic
ENTRYPOINT ["/hasteward"]
