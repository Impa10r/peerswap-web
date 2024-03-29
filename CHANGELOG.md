# Versions

## 1.2.3

- Add fee rate estimate for peg-in
- Allow fee bumping peg-in tx (first CPFP, then RBF)

## 1.2.2

- Fix bug causing log loading delay
- Add Tor proxy (socks5) parameter in config
- Use Tor proxy to connect to Bitcoin Core, Mempool and Telegram
- Use LND to search node aliases first, Mempool as backup

## 1.2.1

- Various UI style improvements

## 1.2.0

- Add Bitcoin balance and utxo list display
- Implement Liquid Peg-In functionality
- Fallback to getblock.io if local Bitcoin Core unreachable

## 1.1.8

!!! BREAKING CHANGES FOR DOCKER !!! 

- Docker image can no longer be run by host's root user
- Using a non-root user "peerswap" inside the container

Migration steps:
1. Create a non-root user if it does not exist, login with it
2. If your .peerswap folder is in /root/, copy it to /home/USER/
3. Take ownershp of data folder with ```sudo chown USER:USER ~/.peerswap -R```
4. Open peerswap.conf and pswebconfig.json for edit, search and replace all "/root/" with "/home/peerswap/"
5. Run the image with the new parameters per [Readme](https://github.com/Impa10r/peerswap-web?tab=readme-ov-file#docker).

## 1.1.7

- Add link on Config page to see PS Web log 
- Add basic menu to Telegram Bot

## 1.1.6

- Zip wallet backups with Elements RPC password
- Save master blinding key as .bak file name
- Enable automatic backups sent to a Telegram Bot

## 1.1.5

- Use Elements RPC to send Liquid
- Allow send max with subtractfeefromamount option
- Default swap direction depends on channel balance

## 1.1.4

- Add a peerswapd log page with a link from config page
- Disallow new swaps if there is an active swap already pending (to avoid peerswap bug 'already has an active swap on channel')
- Log text and liquid address are copied to clipboard when clicking on the respective fields

## 1.1.3

- Add Privacy mode to home page (for screen sharing etc)
- Add peerswapd log output if psweb cannot connect to it

## 1.1.2

- Add unspent outputs list on Liquid page

## 1.1.1

- Add logging to psweb.log file
- BREAKING CHANGE: remove outputs to psweb.log from psweb.service AND DELETE .peerswap/psweb.log before restarting the service!
- Add Liquid wallet backup (see README to configure)

## 1.1.0

- Add Docker support

## 1.0.4

- Default port number change from 8088 to 1984
- Add local mempool option for node alias and bitcoin tx lookups

## 1.0.3

- Change dafault links to mainnet
- Better format peer details 
- Reuse header in swap.gohtml
- Dissolve utils package

## 1.0.2

- Disable autofill and autocomplete for the Liquid send to address

## 1.0.1

- New liquid receiving address now must be requested with a button
- Make amount a required field in the swap and send liquid forms

## 1.0.0

- Initial release into production
