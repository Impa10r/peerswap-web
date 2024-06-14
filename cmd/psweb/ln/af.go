//go:build !cln

package ln

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
)

func ApplyAutoFee(client lnrpc.LightningClient, channelId uint64, failedHTLC bool) {

	if !AutoFeeEnabledAll || !AutoFeeEnabled[channelId] {
		return
	}

	params := &AutoFeeDefaults
	if AutoFee[channelId] != nil {
		// channel has custom parameters
		params = AutoFee[channelId]
	}

	ctx := context.Background()
	if myNodeId == "" {
		// get my node id
		res, err := client.GetInfo(ctx, &lnrpc.GetInfoRequest{})
		if err != nil {
			return
		}
		myNodeId = res.GetIdentityPubkey()
	}
	r, err := client.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanId: channelId,
	})
	if err != nil {
		return
	}

	policy := r.Node1Policy
	peerId := r.Node2Pub
	if r.Node1Pub != myNodeId {
		// the first policy is not ours, use the second
		policy = r.Node2Policy
		peerId = r.Node1Pub
	}

	oldFee := int(policy.FeeRateMilliMsat)
	newFee := oldFee

	if failedHTLC {
		// increase fee to help prevent further failed HTLCs
		newFee += params.FailedBumpPPM
	} else {
		// get balances
		bytePeer, err := hex.DecodeString(peerId)
		if err != nil {
			return
		}

		res, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
			PublicOnly: true,
			Peer:       bytePeer,
		})
		if err != nil {
			return
		}

		localBalance := int64(0)
		for _, ch := range res.Channels {
			if ch.ChanId == channelId {
				localBalance = ch.LocalBalance
				break
			}
		}

		liqPct := int(localBalance * 100 / r.Capacity)
		if liqPct > params.LowLiqPct {
			// normal or high liquidity regime, check if fees can be dropped
			if policy.LastUpdate < uint32(time.Now().Add(-time.Duration(params.CoolOffHours)*time.Hour).Unix()) {
				// check the last outbound timestamp
				if lastForwardTS[channelId] < time.Now().AddDate(0, 0, -params.InactivityDays).Unix() {
					// decrease the fee
					newFee -= params.InactivityDropPPM
					newFee = newFee * (100 - params.InactivityDropPct) / 100
				}

				// check the floors
				if liqPct < params.ExcessPct {
					newFee = max(newFee, params.NormalRate)
				} else {
					newFee = max(newFee, params.ExcessRate)
				}
			}
		} else {
			// liquidity is low, floor the rate at high value
			newFee = max(newFee, params.LowLiqRate)
		}
	}

	// set the new rate
	if newFee != oldFee {
		SetFeeRate(peerId, channelId, int64(newFee), false, false)
	}
}

func ApplyAutoFeeAll() {
	client, cleanup, err := GetClient()
	if err != nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	res, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
		ActiveOnly: true,
	})
	if err != nil {
		return
	}

	for _, ch := range res.Channels {
		ApplyAutoFee(client, ch.ChanId, false)
	}
}
