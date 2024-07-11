# Versions

## 1.6.6

- Retry polling peers for L-BTC balances after peerswap initializes

## 1.6.5

- Reduce frequency of balance announcements to daily unless changed
- Pre-fill 0 if possible swap amount is below 100,000

## 1.6.4

- Implement advertising L-BTC balance: LND send and receive, CLN send
- Pre-fill swap amount to return the channel closer to 50/50 if viable
- Persist NodeId per ChannelId map to avoid *closed channel*
- Apply AutoFees even while HTLC is pending

## 1.6.3

- Fix bug in accounting for non-peerswap related invoices

## 1.6.2

- Fix 'invalid asset' bug
- AutoSwap: disallow spending LBTC gained in the same channel
- AutoSwap: only consider channels with routed out > routed in

## 1.6.1

- AutoFee: add Update All to set paramereter(s) to all custom rules
- Fix forwards subscription to add channel IDs

## 1.6.0

- Make New Swap form inputs more intuitive
- AutoFee: add last forwards history
- Add peer fee rates display on peer page

## 1.5.9

- Add get BTC receiving address and send BTC with coin selection
- AutoFee: add column with days from the last outbound flow
- AutoFee: fix live outbound payments not registering for LND

## 1.5.8

- Ignore forwards < 1000 sats in statistics and autofees 
- Show 7 day Fee Log for individual channels

## 1.5.7

- AutoFee: Fix enable/disable all individual channels
- AutoFee: Fix inbound fee management
- AutoSwap: Add max swap amount limit
- AutoSwap: Disable on error or swap failure
- Add cost of swaps failed with State_ClaimedCoop or State_ClaimedCsv
- Persist swap costs and rebates to db

## 1.5.6

- AutoFee: Add Fee Log table for the last 24 hours 
- AutoFee: Draw current fee rate with a dotted line on chart 

## 1.5.5

- Fix panic
- Add lines to AF chart for High/Normal/Low Liq rates

## 1.5.4

- Hide HTTPS option for Umbrel
- AutoFee: apply HTLC Fail Bumps only when Local % <= Low Liq % 
- AutoFee: for HTLC Fails above Low Liq % allow increasing Low Liq % threshold
- AutoFee: log full fee changes history, including inbound
- AutoFee: reduce LND load when applying auto fees
- AutoFee: add realized PPM chart for the channel to help decide AF parameters

## 1.5.3

- Add automatic channel fees management
- Randomize serial number of CA to let install in Ubuntu/Firefox
- Increase Web UI password cookie expiry to 7 days
- Add NO_HTTPS env variable for Umbrel update

## 1.5.2

- Add navigation menu
- Allow HTTPS with single password client authentication
- Add -password key to configure PSWeb with HTTPS and password 

## 1.5.1

- Enable setting fee rates, including inbound for LND 0.18+
- If failed on startup, keep trying to subscribe every minute
- Better UI colors (I think)

## 1.5.0

- Use hostname of the host in server.crt when run in Docker

## 1.4.9

- Fix HTTPS error when "PSWeb IPs" is blank

## 1.4.8

- Add HTTPS connection with mandatory TLS certificates
- Add swap statistics (Total amount, cost, PPM)

## 1.4.7

- Remove resthost from peerswap.conf
- Fix panic in appendInvoices
- Add revenue and costs to tooltips

## 1.4.6

- Estimate swap fees before submitting
- LND: Fix invoice subscription reconnections 
- Auto swap: account for MaxHtlc settings
- Keysend: account for MinHtlc settings
- Fix swap-out fees missing for some swaps

## 1.4.5

- Add swap costs to home page
- Fix error reporting

## 1.4.4

- Fix panic in v1.4.3

## 1.4.3

- Add http retry middleware
- Add swap costs display

## 1.4.2

- Subscribe to LND events to reduce CPU overhead

## 1.4.1

- Account for paid and received invoices in channel flow statistics
- Show Lightning costs of paid invoices (excluding peerswap's)

## 1.4.0

- Enable viewing non-PeerSwap channels
- Enable keysend invitations to PeerSwap 
- Fix the bug when PS RPC host was replaced with Elements'

## 1.3.7

- Enable automatic Liquid swap-ins
- Report fees paid via LN for initiated swap-outs
- Fix rotating hourglass
- Fix failed swaps shortening flow timeframe 

## 1.3.6

- Fix Elements host and port values passed via Env

## 1.3.5

- Estimate peg-in transaction size, total fee and PPM
- Add peer fee revenue stats to the main page

## 1.3.4

- Display channel flows since their last swaps
- Allow filtering swaps history 
- Speed up loading of pages
- Display current fee rates for channels
- Add help tooltips
- CLN: implement incremental forwards history polling
- LND 0.18+: exact fee when sending change-less peg-in tx (there was a bug in LND below 0.18)

## 1.3.3

- Add channel routing statistics on the peer screen
- Visual improvements for mobile browsers

## 1.3.2

- Add Bitcoin UTXOs selection for Liquid Peg-ins
- Allow deducting peg-in chain fee from the amount to avoid change output
- CLN: Can bump peg-in chain fees with RBF
- LND 0.18+: Can bump peg-in chain fees with RBF
- LND below 0.18: Can bump peg-in chain fees with CPFP

## 1.3.1

- Enable peg-in transaction fee bumping for CLN
- Add LND log link
- Various bug fixes

## 1.3.0

- Enable Core Lightning manual build

## 1.2.6

- Fix bug in conversion to millions
- Add Makefile, update installation instructions

## 1.2.5

- Bug fix for peg-in claim not working in v1.2.4

## 1.2.4

- Retry connecting to Telegram bot every minute if failed on start
- Display rpc connection error in plain English
- Add github workflow to build docker image

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
- It must be run by the user with id 1000
- Running a non-root user "peerswap" with id 1000 inside the container

Migration steps:
1. Create a non-root user if it does not exist, login with it. Otherwise, use user with id 1000 (the first one you created on your node).
2. If your .peerswap folder is in /root/, copy it to /home/USER/
3. Take ownershp of data folder with ```sudo chown USER:USER ~/.peerswap -R```
4. Open peerswap.conf and pswebconfig.json for edit, search and replace all "/root/" with "/home/peerswap/"
5. Run the image with the new parameters per [Readme](https://github.com/Impa10r/peerswap-web/blob/main/README.md#docker-lnd-only).

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

- Change default links to mainnet
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
