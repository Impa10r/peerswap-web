###
## Build the Go App
###
FROM golang:1.21.5-bullseye AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY . .

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /bin/psweb cmd/psweb

FROM debian:bullseye-slim
COPY --from=builder /bin/psweb .

RUN mkdir -p /home/.peerswap

EXPOSE 8088

CMD [ "./psweb" ]
