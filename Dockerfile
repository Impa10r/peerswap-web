###
## Build PeerSwap and PeerSwap Web UI in a joint container 
###

FROM golang:1.21.5-bullseye AS builder

ARG TARGETOS
ARG TARGETARCH
ARG COMMIT

WORKDIR /app

COPY . .

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /go/bin/psweb cmd/psweb/*.go

RUN git clone https://github.com/ElementsProject/peerswap.git && cd peerswap && git checkout $COMMIT

RUN cd peerswap && make -j$(nproc) lnd-release

FROM debian:buster-slim

RUN sed -i 's|$|deb http://deb.debian.org/debian buster main contrib non-free|' /etc/apt/sources.list
RUN apt-get update 

RUN apt-get install -y supervisor
RUN mkdir -p /var/log/supervisor
COPY supervisord.conf /etc/supervisor/conf.d/supervisord.conf

RUN apt-get install -y ca-certificates

RUN cd /root/

COPY --from=builder /go/bin/peerswapd /root/
COPY --from=builder /go/bin/psweb /root/
COPY --from=builder /go/bin/pscli /root/

RUN mkdir -p /root/.peerswap

EXPOSE 1984

CMD ["/usr/bin/supervisord"]
