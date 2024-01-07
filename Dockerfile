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

RUN cd /root/

COPY --from=builder /go/bin/peerswapd .
COPY --from=builder /go/bin/psweb .

RUN mkdir -p /root/.peerswap

EXPOSE 8088

CMD [ "./psweb" ] 
