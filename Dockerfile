# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" -o /out/aninode ./cmd/aninode

FROM alpine:3.24
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="aninode" \
      org.opencontainers.image.description="Deterministic media organization for Emby, Jellyfin, and Plex" \
      org.opencontainers.image.source="https://github.com/Mistarille224/aninode" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="Apache-2.0"
RUN apk add --no-cache su-exec wget ca-certificates
COPY --from=build /out/aninode /usr/local/bin/aninode
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["serve"]
