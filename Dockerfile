# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine3.24 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/zencoder2api .

FROM alpine:3.24

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app \
    && adduser -S -D -H -G app app \
    && mkdir -p /data \
    && chown app:app /data

WORKDIR /app

COPY --from=build /out/zencoder2api ./zencoder2api
COPY web ./web

USER app

ENV PORT=8080 \
    DB_PATH=/data/data.db

VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["./zencoder2api"]
