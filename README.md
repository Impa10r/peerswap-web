# PeerSwap Web UI
Server-side rendered UI to connect to PeerSwap LND daemon. 

# Development

REST API client was generated with https://github.com/go-swagger/go-swagger using https://github.com/ElementsProject/peerswap/blob/master/peerswaprpc/peerswaprpc.swagger.json

```
.\swagger generate client -f peerswaprpc.swagger.json -A peerswap-rest-cli
```
