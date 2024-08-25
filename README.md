![image](https://github.com/Impa10r/peerswap-web/assets/101550606/80f8f9a0-7771-47ad-8484-27c79a8ff37f)

# PeerSwap Web UI

A lightweight server-side rendered Web UI for PeerSwap, which allows trustless p2p submarine swaps Lightning<->BTC and Lightning<->Liquid. Also facilitates BTC->Liquid pegins and automatic channel fee management. PeerSwap with [Liquid](https://help.blockstream.com/hc/en-us/articles/900001408623-How-does-Liquid-Bitcoin-L-BTC-work) is a great cost efficient way to [rebalance lightning channels](https://medium.com/@goryachev/liquid-rebalancing-of-lightning-channels-2dadf4b2397a).

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

Liquid BTC is more custodial than Bitcoin and Lightning. We do not advise accumulating large balances for long-term holding. Once you gained Liquid in a peer swap-in or a pegin process, it is better to initiate own swap in to rebalance a channel of your choice. 

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

# Liquid Pegin

Update: Since v1.2.0 this is handled via UI on the Bitcoin page.

To convert some BTC on your node into L-BTC you don't need any third party (but must run a full Bitcon node with txindex=1, or manually provide block hash from mempool.space for ```getrawtransaction```):

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

## Confidential Liquid Pegin

Elements Core v23.2.2 introduced vsize discount for confidential transactions. Now sending a Liquid payment with a blinded amount costs the same or cheaper than a publicly visible (explicit) one. For example, claiming a pegin with ```elements-cli claimpegin``` costs about 45 sats, but it is possible to manually construct the same transaction (```elements-cli createrawtransaction```) with confidential destination address, blind and sign it, then post and pay a lower fee. However, from privacy perspective, blinding a single pegin claim makes little sense. The linked Bitcoin UTXO will still show the explicit amount, so it is easily traceable to your new Liquid address. To achieve a truly confidential pegin, it is necessary to mix two or more independent claims into one single transaction, a-la CoinJoin.

In v1.7.0 PSWeb implemented such "ClaimJoin". If you opt in when starting your pegin, your node will send invitations to all other PSWeb nodes to join in while you wait for your 102 confirmations. To join your claim, a peer should opt in while starting his own pegin. A node responds to an invitation by anonymously sending details of its pegin funding transaction, once it confirms, to the initiator. Peers don't know which specific node initiated the ClaimJoin and who else will be joining. The initiator also doesn't know public Ids of the nodes that responded. All communication happens blindly via single use public/private key pairs (secp256k1). Nodes who do not directly participate act as p2p relays for the encrypted messages, not being able to read them and not knowing the sources and the final destinations. This way our ClaimJoin coordination is fully confidential and not limited to direct peers. 

When all N pegins mature, the initiator node prepares one large PSET with N pegin inputs and N CT outputs, shuffled randomly, and sends it secuentially to all participants: first to blind Liquid outputs and then to sign pegin inputs. Before blinding/signing and returning the PSET, each joiner verifies that his output address is there for the correct amount (allowing for a small fee haircut). Upto 10 claims can be joined this way, to fit into one custom message (64kb). The price for such privacy is time. For the initiator, the wait can take upto 34 hours if the final peer joins at block 101. For that last joiner the wait will be the same 17 hours as for a standard pegin. If the total fee cannot be divided equally, the last joiner pays slightnly more as an incentive to join earlier next time. In practice, the blinding and signing round may need to be done twice: first to find out the exact discounted vsize of the final transaction, then to set the exact total fee at 0.1 sat/vb. 

The process bears no risk to the participants. If any joiner becomes unresponsive during the blinding/signing round, he is automatically kicked out. If the initiator fails to complete the process, each joiner reverts to a standard single pegin claim 10 blocks after the final maturity. As the last resort, if your PSWeb dies completely, you can always [claim your pegin manually](#liquid-pegin) with ```elements-cli```. All the necessary details will be in your PSWeb log. Your claim script and pegin txid can only be used with your own Liquid wallet's private key. Blinding and signing your part happens locally on your node, no sensitive info is transmitted outside at any point.

# Support

Information about PeerSwap and a link to join their Discord channel is at [PeerSwap.dev](https://peerswap.dev). Additionally, there is a [Telegram group](https://t.me/PeerSwapLN) for node runners with PeerSwap. Just beware of scammers who may DM you. Immediately block and report to Telegram anyone with empty Username field.
