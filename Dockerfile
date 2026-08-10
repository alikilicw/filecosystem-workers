# libvips is a C library, so both stages need it: headers to compile against
# and the shared library to run.
FROM golang:1.25-bookworm AS builder

RUN apt-get update \
    && apt-get install -y --no-install-recommends libvips-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends libvips42 ca-certificates wget \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home filecosystem

COPY --from=builder /out/worker /usr/local/bin/worker

USER filecosystem
EXPOSE 8090

HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=5 \
    CMD wget -qO- http://127.0.0.1:8090/healthz || exit 1

ENTRYPOINT ["worker"]
