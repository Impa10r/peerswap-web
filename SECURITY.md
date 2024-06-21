# Security Protocol

PeerSwap Web UI offers secure communication with the clients via mTLS. When HTTPS option is enabled, a self-signed root Certificate Authority CA.crt is created first. It is then used to sign two certificates: server.crt and client.crt. Both CA.crt and client.crt need to be installed on the client's devices, to bootstrap a secure connection with the server. The certificates are used during the TLS handshake to authenticate the server to the client and vice versa. Our communication channel is now encrypted and no third party can eavesdrop or connect to the server.

For networks with small attack surfaces it is possible to opt-in for a less secure setup with a single client password instead of the client.crt certificate. In this case a session browser cookie is used to maintain authentication status. Warning: without CA certificate installed on the user device the browser will display warnings that the server cannot be trusted. This is because MITM attack is possible, where another server pretends to be PeerSwap Web UI to phish the password. Always make sure to install the CA certificate when opting for password authentication.  

## Privacy Disclosure

There is no centralized server. PeerSwap Web UI does not share your private data with the contributors. The software, however, may utilize API endpoints of github.com, mempool.space, telegram.org and getblock.io to send and receive certain information. You can avoid leaking your IP address to these websites by specifying a Tor proxy on the Configuration page. You may also provide URL of a locally installed Mempool server. 

Getblock runs a publicly available Bitcoin Core server. It is used as a fallback when your local installation is not accessible via API or is not configured to enable Liquid peg-ins. The default account is anonymous, but some contributors may have access to monitor usage statistics. You may opt out by registering your own free account at getblock.io and providing its endpoint, or by running your local suitably configured Bitcoin Core software.

## Reporting a Vulnerability

If you discover any vulnerability, please report it discretely to contributors.
