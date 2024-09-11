# Security Protocol

PeerSwap Web UI offers secure communication with the clients via mTLS. When HTTPS option is enabled, a self-signed root Certificate Authority CA.crt is created first. It is then used to sign two certificates: server.crt and client.p12. Both CA.crt and client.p12 need to be installed on the client's devices, to bootstrap a secure connection with the server. The certificates are used during the TLS handshake to authenticate the server to the client and vice versa. Our communication channel is now encrypted and no third party can eavesdrop or connect to the server.

For networks with small attack surfaces it is possible to opt-in for a less secure setup with a single client password instead of the client.crt certificate. In this case a session browser cookie is used to maintain authentication status. Warning: without CA certificate installed on the user device the browser will display warnings that the server cannot be trusted. This is because MITM attack is possible, where another server pretends to be PeerSwap Web UI to phish the password. Always make sure to install the CA certificate when opting for password authentication.  

## Privacy Disclosure

There is no centralized server. PeerSwap Web UI does not share your private data with the contributors nor send them any donations. The software, however, may utilize API endpoints of github.com, mempool.space, telegram.org and getblock.io to send and receive certain information. You can avoid leaking your IP address to these websites by specifying a Tor proxy on the Configuration page. You may also provide URL of a locally installed Mempool server.

Getblock runs a publicly available Bitcoin Core server. It is used as a fallback when your local installation is not accessible via API or is not configured to enable Liquid peg-ins. The default account is anonymous, but some contributors may have access to monitor aggregate usage statistics. You may opt out by registering your own free account at getblock.io and providing its endpoint, or by running your suitably configured Bitcoin Core locally.

BTC and L-BTC on-chain balances are advertised to direct peers only to the extent they can be discovered by brute force. A peer can attempt smaller and smaller size swap-outs until one works, resulting in many annoying failed swaps in the history. He cannot do swaps larger than his local channel balance and smaller than 100k sats. To mirror that, your advertised balance will be capped at your remote channel balance and rounded down to 0 if below 100k. This way you will not disclose more than what is potentially discoverable. Advertising balances does not reduce your privacy and is therefore enabled by default (but can be easily disabled).

## Reporting Vulnerability

If you discover a vulnerability, please report it to email impalor@pm.me.

Sensitive information should be encrypted with this [PGP key](https://gist.github.com/Impa10r/33b09271ac8ae3f1545cf78318369810) (fingerprint FDD6 C5A6 EAD5 F4F2 3404  27FB 770B CCFB 70F3 133D).