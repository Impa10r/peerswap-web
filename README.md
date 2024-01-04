![image](https://github.com/Impa10r/peerswap-web/assets/101550606/2eeedf98-0d57-42db-83e8-b0f2df2f93f2)

# PeerSwap Web UI

A lightweight server-side rendered Web UI for PeerSwap LND, which allows trustless P2P submarine swaps Lightning<->BTC and Lightning<->Liquid BTC. PeerSwap with Liquid is a great cost efficient way to [rebalance lightning channels](https://medium.com/@goryachev/liquid-rebalancing-of-lightning-channels-2dadf4b2397a).

# Setup

## Install dependencies

PeerSwap Web UI requires Bitcoin Core, Elements Core, LND and PeerSwap for LND. Please consult [these instructions](https://github.com/ElementsProject/peerswap/blob/master/docs/setup_lnd.md) to install PeerSwap.

Install golang from https://go.dev/doc/install

## Build

Clone the repository and build PeerSwap Web UI:

```bash
git clone https://github.com/Impa10r/peerswap-web && \
cd peerswap-web/cmd/psweb && \
go install
```

This will install `psweb` to your GOPATH (/home/USER/go/bin). You can check that it is working by running `psweb --version`. If not, add the bin path in .profile and reload with `source .profile`.

To start psweb as a daemon, create a systemd service file as follows (replace USER with your username):

```bash
sudo nano /etc/systemd/system/psweb.service
```
```
[Unit]
Description=PeerSwap Web UI
Requires=peerswapd.service
After=peerswapd.service

[Service]
ExecStart=/home/USER/go/bin/psweb
User=USER
Type=simple
KillMode=process
TimeoutSec=180
Restart=always
RestartSec=5

StandardOutput=append:/home/USER/.peerswap/psweb.log
StandardError=append:/home/USER/.peerswap/psweb.log

[Install]
WantedBy=multi-user.target
```
Save with ctrl-S, exit with ctrl-X

Now start the service, check that it runs, then enable it on startup:

```bash
sudo systemctl start psweb
sudo systemctl status psweb
sudo systemctl enable psweb
```

The log and the config file will be saved to ~/.peerswap/ folder. 

## Configuration

By default, PeerSwap Web UI will listen on [localhost:8088](localhost:8088). This port can be changed in ~/.peerswap/pswebconf.json.

It is agnostic to whether your LND and Elements are running on testnet/signet or mainnet. It has interface only to peerswapd via RPC. Once opened the UI, set the Links on the Config page for testnet or mainnet.

## Update

When a new version comes out, just build the app again and restart:

```bash
rm -rf peerswap-web && \
git clone https://github.com/Impa10r/peerswap-web && \
cd peerswap-web/cmd/psweb && \
go install && \
sudo systemctl restart psweb
```

## Uninstall

Stop and disable the service:

```bash
sudo systemctl stop psweb
sudo systemctl disable psweb
```

# Support

Information about PeerSwap and a link to join our Discord channel is at [PeerSwap.dev](https://peerswap.dev).

# Security Disclosure

**Assuming the local network is secure**

PeerSwap Web is currently a beta-grade software that makes the assumption that the local network is secure. This means local network communication is unencrypted using plain text HTTP. 

This is pretty much the industry standard when it comes to locally networked devices. All routers and smart devices that expose a web interface work this way. Bootstrapping a secure connection over an insecure network and avoiding MITM attacks without being able to rely on certificate authorities is not an easy problem to solve.

*Only recommend using with small balances or on signet/testnet*

*See [license](/LICENSE) for other disclaimers*
