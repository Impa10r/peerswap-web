# PeerSwap Web UI
Server-side rendered UI to connect to PeerSwap LND daemon. 

# Instalation

# Development
You will need to create an `.env` file on the root dir. An easy way of doing this is just to copy the sample file to `.env`.

```
cp .env-sample .env
```

REST API client was generated with https://github.com/go-swagger/go-swagger using https://github.com/ElementsProject/peerswap/blob/master/peerswaprpc/peerswaprpc.swagger.json

```
.\swagger generate client -f peerswaprpc.swagger.json -A peerswap-rest-cli
```
dlv debug ~/go/src/peerswap-web/cmd/web/main.go --headless --listen=:3000 --log