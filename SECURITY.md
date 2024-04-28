# Security Disclosure

**Assuming the local network is secure**

PeerSwap Web UI is currently a beta-grade software that makes the assumption that the local network is secure. This means local network communication is unencrypted using plain text HTTP. 

Bootstrapping a secure connection over an insecure network and avoiding MITM attacks without being able to rely on certificate authorities is not an easy problem to solve.

## Privacy Disclosure

There is no centralized server. PeerSwap Web UI does not share your private data with the developers. The software, however, may utilize API endpoints of mempool.space, telegram.org and getblock.io to send and receive certain information. You can avoid leaking your IP address to these websites by specifying a Tor proxy on the Configuration page. You may also provide URL of a locally installed Mempool server. 

Getblock runs a publicly available Bitcoin Core server. It is used as a fallback when your local installation is not accessible via API or is not configured to enable Liquid peg-ins. The default account is anonymous, but the developers of PeerSwap Web UI do have access to monitor usage statistics. You may opt out by registering your own free account at getblock.io and providing its endpoint, or by running your local suitably configured Bitcoin Core software.

## Reporting a Vulnerability

If you discover any vulnerability, please contact me directly at t.me/vladgoryachev
