###
## Build PeerSwap and PeerSwap Web UI in a joint container
###

FROM golang:1.25.12-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH
ARG COMMIT

WORKDIR /app

# Copy only module files first so `go mod download` layer is cached
# independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    git clone --filter=blob:none https://github.com/ElementsProject/peerswap.git /peerswap && \
    cd /peerswap && \
    git checkout $COMMIT && \
    cd /app && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} make -j$(nproc) install-lnd && \
    cd /peerswap && \
    make -j$(nproc) lnd-release

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends supervisor ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /var/log/supervisor

COPY supervisord.conf /etc/supervisor/conf.d/supervisord.conf
COPY --from=builder /go/bin/* /bin/

RUN useradd -rm -s /bin/bash -u 1000 -U peerswap
USER peerswap

EXPOSE 1984
EXPOSE 1985

CMD ["/usr/bin/supervisord"]