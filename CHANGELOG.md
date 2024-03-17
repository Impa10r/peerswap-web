# Versions

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
