![image](https://github.com/Impa10r/peerswap-web/assets/101550606/80f8f9a0-7771-47ad-8484-27c79a8ff37f)

# PeerSwap Web UI

A lightweight server-side rendered Web UI for PeerSwap, which allows trustless p2p submarine swaps Lightning<->BTC and Lightning<->Liquid. Also facilitates BTC->Liquid peg-ins and automatic channel fee management. PeerSwap with [Liquid](https://help.blockstream.com/hc/en-us/articles/900001408623-How-does-Liquid-Bitcoin-L-BTC-work) is a great cost efficient way to [rebalance lightning channels](https://medium.com/@goryachev/liquid-rebalancing-of-lightning-channels-2dadf4b2397a).

### Disclaimer

This source code is free speech. The contributors do not solicit its use for any purpose, do not control, are not responsible for and gain nothing from such use.

# Setup

## Install dependencies

PeerSwap requires Bitcoin Core installed and synced, Elements Core installed and synced, LND or Core Lightning installed.

## Docker (LND only)

```
mkdir -p ~/.peerswap && \
docker run --net=host \
--user 1000:1000 \
-v ~/.lnd:/home/peerswap/.lnd:ro  \
-v ~/.elements:/home/peerswap/.elements:ro \
-v ~/.peerswap:/home/peerswap/.peerswap \
-e ELEMENTS_FOLDER="/home/$(whoami)/.elements" \
-e ELEMENTS_FOLDER_MAPPED="/home/peerswap/.elements" \
-e HOSTNAME="$(hostname)" \
-e NETWORK="testnet" \
ghcr.io/impa10r/peerswap-web:latest
```

This example assumes .lnd and .elements folders in the host user's home directory, and connects to LND via host network. Change "testnet" to "mainnet" for production.

Config files should exist or wiil be created with default values. Depending on how your LND and Elements Core are actually installed, may require different parameters (-e). If -e NETWORK="testnet" is ommitted, mainnet assumed. See [Umbrel integration](https://github.com/getumbrel/umbrel-apps/blob/master/peerswap/docker-compose.yml) for all supported env variables.

If you need to run pscli in the docker container, first lookup container id with ```docker ps```. Then run ```docker exec "container id" pscli```.

If Elements is also run in a Docker container, it should be started by the same user as the PeerSwap one (user id 1000:1000).

Please note that configuration files of the Docker version are not compatible with the manual build.

## Manual Build

Install golang from https://go.dev/doc/install

Install and configure PeerSwap. Please consult [these instructions for LND](https://github.com/ElementsProject/peerswap/blob/master/docs/setup_lnd.md) and [these for CLN](https://github.com/ElementsProject/peerswap/blob/master/docs/setup_cln.md).

Clone the repository and build PeerSwap Web UI:

### LND:

```bash
git clone https://github.com/Impa10r/peerswap-web && \
cd peerswap-web && \
make -j$(nproc) install-lnd
```

### CLN:

```bash
git clone https://github.com/Impa10r/peerswap-web && \
cd peerswap-web && \
make -j$(nproc) install-cln
```

This will install `psweb` to your GOPATH (/home/USER/go/bin). You can check that it is working by running `psweb --version`. If not, add the path in ~/.profile and reload with `source .profile`.

To start psweb as a daemon, create a systemd service file as follows (replace USER with your username):

```bash
sudo nano /etc/systemd/system/psweb.service
```
```
[Unit]
Description=PeerSwap Web UI

[Service]
ExecStart=/home/USER/go/bin/psweb
User=USER
Type=simple
KillMode=process
TimeoutSec=180
Restart=always
RestartSec=5

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

The log and the config file will be saved to peerswap folder (```~/.peerswap``` for LND and ```~/.lightning/bitcoin/peerswap``` for CLN). 

## Configuration

By default, PeerSwap Web UI will listen on [localhost:1984](localhost:1984). This port can be changed in ```pswebconfig.json```.

Once opened the UI, set the Links on the Config page for testnet or mainnet. If an environment variable NETWORK is present and equals "testnet", the links will be configured automatically for testnet on the first run.

To enable downloading of a backup file of the Elements wallet it is necessary to have access to .elements folder where this backup is saved by elementsd. If Elements is run in a Docker container, both the internal folder (usually /home/elements/.elements) and the mapped external folder (for Umbrel it is /home/umbrel/umbrel/app-data/elements/data) must be provided in the Configuration panel.

***Warning*** If you tried PS Web's Docker version first and then switched to the one built from source, the configuration files will be incorrect. The easiest way to fix this is to delete ```peerswap.conf``` and ```pswebconfig.json```.

PeerSwap Web UI can be initialized in HTTPS mode with a pre-set password using -password key. CA and server certificates will be generated and saved in the data folder. See more [here](https://github.com/Impa10r/peerswap-web/blob/main/SECURITY.md).

## Update

When a new version comes out, just build the app again and restart:

### LND:

```bash
rm -rf peerswap-web && \
git clone https://github.com/Impa10r/peerswap-web && \
cd peerswap-web && \
make -j$(nproc) install-lnd && \
sudo systemctl restart psweb
```

### CLN:

```bash
rm -rf peerswap-web && \
git clone https://github.com/Impa10r/peerswap-web && \
cd peerswap-web && \
make -j$(nproc) install-cln && \
sudo systemctl restart psweb
```

## Automatic Liquid Swap-Ins

Liquid BTC is more custodial than Bitcoin and Lightning. We do not advise accumulating large balances for long-term holding. Once you gained Liquid in a peer swap-in or a peg-in process, it is better to initiate own swap in to rebalance a channel of your choice. 

Currently, it is not possible to prevent swap outs by other peers while allowing receipt of swap ins. You don't want your Liquid balance taken, because such a rebalancing may not be optimal for you (but optimal for your peer).

You may enable automatic deployment of L-BTC deposits, as soon as they arrive, to rebalance your most profitable channels. This option is available from Liquid page.

## Liquid wallet backup and restore

**Elements Core wallet has no seed phrase recovery**

Take care to backup PeerSwap wallet after each Liquid transaction. In case of a catastrophic failure of your SSD all L-BTC funds may be lost. Always run your node with a UPS. 

Backup is done from the Liquid page. The file name ```<hexkey>.bak``` is your master blinding key hex. The contents is the backup of wallet.dat. For safety, this .bak will be zipped with the same password as the Elements RPC. For Umbrel, this is the password displayed in the Elements Core App. For the rest, this is the .elements/elements.conf rpcpassword parameter.

**Make sure you keep this password safe in a separate location!** 

Restoring the wallet will require elements-cli command line skills:

```bash
elements-cli restorewallet "wallet_name" "backup_file"
```

Default wallet_name is "peerswap", the same value as in peerswap.conf elementsd.rpcwallet parameter.

To restore your master blinding key use:

```bash
elements-cli importmasterblindingkey "hexkey"
```

For Umbrel and other Docker installs it will be necessary first to copy the backup file inside the container:

```bash
docker cp /home/umbrel/peerswap.bak elements_node_1:/home/elements/peerswap.bak
docker exec -it elements_node_1 elements-cli -rpcuser=elements -rpcpassword=49...d1e restorewallet "peerswap" "/home/elements/peerswap.bak"
docker exec -it elements_node_1 elements-cli -rpcuser=elements -rpcpassword=49...d1e importmasterblindingkey "hexkey"
```

**DO NOT** uninstall Elements Core unless all liquid funds are spent or you have backed up your wallet.

## Automatic backup to a Telegram bot

Create a new telegram bot with [BotFather](https://t.me/botfather) and copy API Token to PS Web Configuration page. Type /start. The backup file will be sent upon every change of the Liquid balance. To re-use an existing bot make sure to revoke old API Token.

## Uninstall

Stop and disable the service:

```bash
sudo systemctl stop psweb
sudo systemctl disable psweb
```

# Liquid Peg-In

Update: From v1.2.0 this is handled via UI on the Bitcoin page.

To convert some BTC on your node into L-BTC you don't need any third party (but must run a full Bitcon node with txindex=1 enabled):

1. Generate a special BTC address: ```elements-cli getpeginaddress```. Save claim_script for later.
2. Send BTC onchain: ```lncli sendcoins --amt <sats to peg in> -addr <mainchain_address from step 1> --sat_per_vbyte <from mempool>```
3. Wait for 102 confirmations (about 17 hours). 
4. Run ```bitcoin-cli getrawtransaction <txid from step 2>```
5. Run ```bitcoin-cli gettxoutproof '["<txid from step 2>"]'```
6. Run ```elements-cli claimpegin <raw from step 4> <proof from step 5> <claim_script from step 1>```
7. Your Liquid balance should update once the tx confirms (1-2 minutes)

Taken from [here](https://help.blockstream.com/hc/en-us/articles/900000632703-How-do-I-peg-in-BTC-to-the-Liquid-Network-). 

*Hint for Umbrel:* To save keystrokes, add these aliases to ~/.profile, then ```source .profile```
```
alias lncli="docker exec -it lightning_lnd_1 lncli --lnddir /home/umbrel/umbrel/app-data/lightning/data/lnd"
alias bcli="docker exec -it bitcoin_bitcoind_1 bitcoin-cli -rpcuser=umbrel -rpcpassword=<your bitcoin password>"
alias ecli="docker exec -it elements_node_1 elements-cli -rpcuser=elements -rpcpassword=<your elements password>"
```

(lookup Elements and Bitcoin rpc passwords in pswebconfig.com)

# Support

Information about PeerSwap and a link to join our Discord channel is at [PeerSwap.dev](https://peerswap.dev). Additionally, there is a [Telegram group](https://t.me/PeerSwapLN) for node runners with PeerSwap. Just beware of scammers who may DM you. Immediately block and report to Telegram anyone with empty Username field.
