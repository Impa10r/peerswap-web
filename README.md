![image](https://github.com/Impa10r/peerswap-web/assets/101550606/dce5120d-78ab-473d-b13c-71e55d3f6995)

# PeerSwap Web UI

A lightweight server-side rendered Web UI for PeerSwap LND, which allows trustless p2p submarine swaps Lightning<->BTC and Lightning<->Liquid BTC. PeerSwap with Liquid is a great cost efficient way to [rebalance lightning channels](https://medium.com/@goryachev/liquid-rebalancing-of-lightning-channels-2dadf4b2397a).

# Setup

## Install dependencies

PeerSwap Web UI requires Bitcoin Core, Elements Core and LND.

## Docker

docker run --net=host -v ~/.lnd:/root/.lnd -v ~/.peerswap:/root/.peerswap -e NETWORK='testnet' ghcr.io/impa10r/peerswap-web:v1.1.0

Container includes both peerswapd and peerswap-web, started by supervisord. This example links to .lnd and .peerswap folders in the host machine's home directory, and connects to LND via host network. Config files should exist or wiil be created with default values. Depending on how your LND and Elements Core are actually installed, may require further parameters (-e). If NETWORK is ommitted, mainnet assumed. See [Umbrel integration](https://github.com/Impa10r/umbrel-apps/blob/master/peerswap/docker-compose.yml) for supported env variables.


## Manual Build

Install golang from https://go.dev/doc/install

Install and configure PeerSwap for LND. Please consult [these instructions](https://github.com/ElementsProject/peerswap/blob/master/docs/setup_lnd.md).

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

By default, PeerSwap Web UI will listen on [localhost:1984](localhost:1984). This can be changed in ~/.peerswap/pswebconf.json or via setting environment variables PRC_PORT and PRC_HOST on first run.

It is agnostic to whether your LND and Elements are running on testnet/signet or mainnet. It has interface only to peerswapd via RPC. Once opened the UI, set the Links on the Config page for testnet or mainnet. If an environment variable TESTNET is present and equals "testnet", the links will be configured automatically on first run.

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

**Elements Core wallet has no seed phrase recovery**

Take care to backup wallet.dat after each liquid transaction, including all peer swaps. In case of a catastrophic failure of your SSD all L-BTC funds may be lost. On Umbrel, this file is located in /home/umbrel/umbrel/app-data/elements/data/liquidv1/wallets/peerswap folder. Always run your node with UPS. **DO NOT** uninstall Elements Core unless all liquid funds are spent or you have saved the wallet.dat file. **DO NOT** let this file get into wrong hands, as it is not passphrase protected. 

*Only recommend using with small balances or on signet/testnet*

*See [license](/LICENSE) for other disclaimers*
