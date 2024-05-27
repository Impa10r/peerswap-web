# Security Protocol

PeerSwap Web UI HTTP server offers secure communication with the clients via TLS. When HTTPS option is enabled, a self-signed root Certificate Authority certificate is created first. It is then used to sign two certificates: server.crt and client.crt. Both CA.srt and client.crt need to be installed on the client's PC or mobile, to bootstrap a secure connection with the server. The server.crt certificate is provided during TLS handshake to authenticate the server. The communication channel is now encrypted and no third party can eavesdrop or connect.

## Privacy Disclosure

There is no centralized server. PeerSwap Web UI does not share your private data with the contributors. The software, however, may utilize API endpoints of github.com, mempool.space, telegram.org and getblock.io to send and receive certain information. You can avoid leaking your IP address to these websites by specifying a Tor proxy on the Configuration page. You may also provide URL of a locally installed Mempool server. 

Getblock runs a publicly available Bitcoin Core server. It is used as a fallback when your local installation is not accessible via API or is not configured to enable Liquid peg-ins. The default account is anonymous, but some contributors may have access to monitor usage statistics. You may opt out by registering your own free account at getblock.io and providing its endpoint, or by running your local suitably configured Bitcoin Core software.

## Reporting a Vulnerability

If you discover any vulnerability, please contact me directly at t.me/vladgoryachev
