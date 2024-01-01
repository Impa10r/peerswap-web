![image](https://github.com/Impa10r/peerswap-web/assets/101550606/16f8a697-d0d7-4905-923c-4a1490ae0e63)

# PeerSwap Web
A lightweight server-side rendered Web UI for PeerSwap LND, which allows trustless P2P submarine swaps Lightning-BTC and Lightning-LBTC. 

PeerSwap over Liquid is a great cost effective way to [rebalance you lightning channels](https://medium.com/@goryachev/liquid-rebalancing-of-lightning-channels-2dadf4b2397a). PeerSwap Web is agnostic to whether you are running on testnet/signet or mainnet. It has interface only to peerswapd via gRPC. 

# Setup

## Install dependencies

PeerSwap Web requires Bitcoin Core, Elements Core (optional, but highly recommented), LND and PeerSwap for LND. Please consult [these instructions](https://github.com/ElementsProject/peerswap/blob/master/docs/setup_lnd.md) to install PeerSwap.

Install golang from https://go.dev/doc/install

## Build

Clone the repository and build PeerSwap Web

```bash
git clone https://github.com/Impa10r/peerswap-web && \
cd peerswap-web/cmd/psweb && \
go install
```

This will install `psweb` to your GOPATH (default: /home/user/go/bin)

To start PS Web as a daemon, create a systemd service file as follows (replace USER with your username):

```
sudo nano /etc/systemd/system/psweb.service

[Unit]
Description=PeerSwap Web
Requires=peerswapd.service
After=peerswapd.service

[Service]
ExecStart=/home/USER/go/bin/psweb
User=USER
Type=simple
KillMode=process
TimeoutSec=180
Restart=always
RestartSec=60

StandardOutput=append:/home/USER/.peerswap/psweb.log
StandardError=append:/home/USER/.peerswap/psweb.log

[Install]
WantedBy=multi-user.target
```
Save with ctrl-S, exit with ctrl-X

Now start the service, check that it runs, then enable it on startup:
```
sudo systemctl start psweb
sudo systemctl status psweb
sudo systemctl enable psweb
```

The log and the config file will be saved to /home/USER/.peerswap/ folder. 
By default, it will listen on [localhost:8088](localhost:8088).

You can see help with:
```
$ psweb --help
Usage:
  -configfile string
        Path/filename to store config JSON
  -help
        Show help
  -version
        Show version
```

# Security Disclosure

**Assuming the local network is secure**

PeerSwap Web is currently a beta-grade software that makes the assumption that the local network is secure. This means local network communication is unencrypted using plain text HTTP. 

This is pretty much the industry standard when it comes to locally networked devices. All routers and smart devices that expose a web interface work this way. Bootstrapping a secure connection over an insecure network and avoiding MITM attacks without being able to rely on certificate authorities is not an easy problem to solve.

*Only recommend using with small balances or on signet/testnet*

*See [license](/LICENSE) for other disclaimers*
