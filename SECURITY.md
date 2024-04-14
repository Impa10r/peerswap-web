# Security Disclosure

**Assuming the local network is secure**

PeerSwap Web UI is currently a beta-grade software that makes the assumption that the local network is secure. This means local network communication is unencrypted using plain text HTTP. 

Bootstrapping a secure connection over an insecure network and avoiding MITM attacks without being able to rely on certificate authorities is not an easy problem to solve.

## Privacy Disclosure

PeerSwap Web UI may dial API endpoints of mempool.space, telegram.org and getblock.io. You can eliminate leaking your IP address by utilizing and specifying a Tor proxy. You may also specify API endpoint of a locally installed Mempool. 

Getblock runs a publicly available Bitcoin Core. It is used as a fallback when your local installation is not accessible via API or is not configured to enable Liquid peg-ins. The default account is anonymous, but the author of PeerSwap Web UI has access to monitor some aggregate usage statistics. You may opt out by registering your own free account and providing its endpoint or by running your own suitably configured Bitcoin Core.

## Reporting a Vulnerability

If you discover any vulnerability, please contact me directly at t.me/vladgoryachev
