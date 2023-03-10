package impl

import (
	"bytes"
	"context"
	"fmt"
	"github.com/filecoin-project/boost/storagemarket/types"
	"github.com/filecoin-project/go-address"
	cborutil "github.com/filecoin-project/go-cbor-util"
	"github.com/filecoin-project/go-fil-markets/shared"
	"github.com/filecoin-project/go-state-types/builtin/v9/market"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/api/client"
	types2 "github.com/filecoin-project/lotus/chain/types"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/lotus-web3/ribs/ributil"
	"golang.org/x/xerrors"
	"time"
)

const DealStatusV12ProtocolID = "/fil/storage/status/1.2.0"
const clientReadDeadline = 10 * time.Second
const clientWriteDeadline = 10 * time.Second

var DealCheckInterval = 10 * time.Second

func (r *ribs) dealTracker(ctx context.Context) {
	gw, closer, err := client.NewGatewayRPCV1(ctx, "http://api.chain.love/rpc/v1", nil)
	if err != nil {
		panic(err)
	}
	defer closer()

	for {
		checkStart := time.Now()
		select {
		case <-r.close:
			return
		default:
		}

		err := r.runDealCheckLoop(ctx, gw)
		if err != nil {
			log.Errorw("deal check loop failed", "error", err)
		}

		checkDuration := time.Since(checkStart)
		if checkDuration < DealCheckInterval {
			select {
			case <-r.close:
				return
			case <-time.After(DealCheckInterval - checkDuration):
			}
		}
	}
}

func (r *ribs) runDealCheckLoop(ctx context.Context, gw api.Gateway) error {
	gw, closer, err := client.NewGatewayRPCV1(ctx, "http://api.chain.love/rpc/v1", nil)
	if err != nil {
		return xerrors.Errorf("creating gateway rpc client: %w", err)
	}
	defer closer()

	/* PUBLISHED DEAL CHECKS */
	/* Wait for published deals to become active (or expire) */

	{
		toCheck, err := r.db.PublishedDeals()
		if err != nil {
			return xerrors.Errorf("get inactive published deals: %w", err)
		}

		for _, deal := range toCheck {
			dealInfo, err := gw.StateMarketStorageDeal(ctx, deal.DealID, types2.EmptyTSK)
			if err != nil {
				return xerrors.Errorf("get deal info: %w", err)
			}

			if dealInfo.State.SectorStartEpoch > 0 {
				if err := r.db.UpdateActivatedDeal(deal.DealUUID, dealInfo.State.SectorStartEpoch); err != nil {
					return xerrors.Errorf("marking deal as active: %w", err)
				}
				log.Infow("deal active", "deal", deal.DealUUID, "dealid", deal.DealUUID, "startepoch", dealInfo.State.SectorStartEpoch)
			}
		}
	}

	/* PUBLISHING DEAL CHECKS */
	/* Wait for publish at "good-enough" finality */

	{
		cdm := ributil.CurrentDealInfoManager{CDAPI: gw}

		toCheck, err := r.db.PublishingDeals()
		if err != nil {
			return xerrors.Errorf("get inactive publishing deals: %w", err)
		}

		head, err := gw.ChainHead(ctx) // todo lookback
		if err != nil {
			return xerrors.Errorf("get chain head: %w", err)
		}

		for _, deal := range toCheck {
			var dprop market.ClientDealProposal
			if err := dprop.UnmarshalCBOR(bytes.NewReader(deal.Proposal)); err != nil {
				return xerrors.Errorf("unmarshaling proposal: %w", err)
			}

			pcid, err := cid.Decode(deal.PublishCid)
			if err != nil {
				return xerrors.Errorf("decode publish cid: %w", err)
			}

			// todo somewhere here we'll need to handle published, failed deals

			cdi, err := cdm.GetCurrentDealInfo(ctx, head.Key(), &dprop.Proposal, pcid)
			if err != nil {
				log.Errorw("get current deal info", "error", err)
				continue
			}

			pubH, err := gw.ChainGetTipSet(ctx, cdi.PublishMsgTipSet)
			if err != nil {
				return xerrors.Errorf("get publish tipset: %w", err)
			}

			if head.Height()-pubH.Height() > dealPublishFinality {
				if err := r.db.UpdatePublishedDeal(deal.DealUUID, cdi.DealID, cdi.PublishMsgTipSet); err != nil {
					return xerrors.Errorf("marking deal as published: %w", err)
				}
				log.Infow("deal published", "deal", deal.DealUUID, "dealid", cdi.DealID, "publishcid", pcid, "publishheight", pubH.Height(), "headheight", head.Height(), "finality", head.Height()-pubH.Height())
			}
		}
	}

	/* PROPOSED DEAL CHECKS */
	/* Mainly waiting to get publish deal cid */

	walletAddr, err := r.wallet.GetDefault()
	if err != nil {
		return xerrors.Errorf("get wallet address: %w", err)
	}

	{
		toCheck, err := r.db.InactiveDealsToCheck()
		if err != nil {
			return xerrors.Errorf("get inactive deals: %w", err)
		}

		// todo also check "failed" not expired deals at some lower interval

		for _, deal := range toCheck {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := r.runDealCheckQuery(ctx, gw, walletAddr, deal)
			cancel()
			if err != nil {
				log.Errorw("deal check failed", "error", err)
			}
		}
	}

	/* Active deal checks */

	// get deals with deal id, active, not checked in last 24 hours

	// check market deal state

	return nil
}

func (r *ribs) runDealCheckQuery(ctx context.Context, gw api.Gateway, walletAddr address.Address, deal inactiveDealMeta) error {
	maddr, err := address.NewIDAddress(uint64(deal.ProviderAddr))
	if err != nil {
		return xerrors.Errorf("new id address: %w", err)
	}

	dealUUID, err := uuid.Parse(deal.DealUUID)
	if err != nil {
		return xerrors.Errorf("parse deal uuid: %w", err)
	}

	addrInfo, err := GetAddrInfo(ctx, gw, maddr)
	if err != nil {
		return xerrors.Errorf("get addr info: %w", err)
	}

	if err := r.host.Connect(ctx, *addrInfo); err != nil {
		return xerrors.Errorf("connect to miner: %w", err)
	}

	resp, err := r.sendDealStatusRequest(ctx, addrInfo.ID, dealUUID, walletAddr)
	if err != nil {
		return fmt.Errorf("send deal status request failed: %w", err)
	}

	if resp.DealStatus != nil {
		if err := r.db.UpdateSPDealState(dealUUID, *resp); err != nil {
			return xerrors.Errorf("storing deal state response: %w", err)
		}
	}

	return nil
}

func (r *ribs) sendDealStatusRequest(ctx context.Context, id peer.ID, dealUUID uuid.UUID, caddr address.Address) (*types.DealStatusResponse, error) {
	log.Debugw("send deal status req", "deal-uuid", dealUUID, "id", id)

	uuidBytes, err := dealUUID.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("getting uuid bytes: %w", err)
	}

	sig, err := r.wallet.WalletSign(ctx, caddr, uuidBytes, api.MsgMeta{Type: api.MTDealProposal})
	if err != nil {
		return nil, fmt.Errorf("signing uuid bytes: %w", err)
	}

	// Create a libp2p stream to the provider
	s, err := shared.NewRetryStream(r.host).OpenStream(ctx, id, []protocol.ID{DealStatusV12ProtocolID})
	if err != nil {
		return nil, err
	}

	defer s.Close() // nolint

	// Set a deadline on writing to the stream so it doesn't hang
	_ = s.SetWriteDeadline(time.Now().Add(clientWriteDeadline))
	defer s.SetWriteDeadline(time.Time{}) // nolint

	// Write the deal status request to the stream
	req := types.DealStatusRequest{DealUUID: dealUUID, Signature: *sig}
	if err = cborutil.WriteCborRPC(s, &req); err != nil {
		return nil, fmt.Errorf("sending deal status req: %w", err)
	}

	// Set a deadline on reading from the stream so it doesn't hang
	_ = s.SetReadDeadline(time.Now().Add(clientReadDeadline))
	defer s.SetReadDeadline(time.Time{}) // nolint

	// Read the response from the stream
	var resp types.DealStatusResponse
	if err := resp.UnmarshalCBOR(s); err != nil {
		return nil, fmt.Errorf("reading deal status response: %w", err)
	}

	log.Debugw("received deal status response", "id", resp.DealUUID, "status", resp.DealStatus)

	return &resp, nil
}
