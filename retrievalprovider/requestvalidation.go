package retrievalprovider

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/filecoin-project/go-address"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/migrations"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	selectorparse "github.com/ipld/go-ipld-prime/traversal/selector/parse"
	peer "github.com/libp2p/go-libp2p/core/peer"

	"github.com/ipfs-force-community/droplet/v2/config"
	"github.com/ipfs-force-community/droplet/v2/models/repo"

	types "github.com/filecoin-project/venus/venus-shared/types/market"
)

var allSelectorBytes []byte

var askTimeout = 5 * time.Second

func init() {
	buf := new(bytes.Buffer)
	_ = dagcbor.Encode(selectorparse.CommonSelector_ExploreAllRecursively, buf)
	allSelectorBytes = buf.Bytes()
}

// ProviderRequestValidator validates incoming requests for the Retrieval Provider
type ProviderRequestValidator struct {
	cfg           *config.MarketConfig
	storageDeals  repo.StorageDealRepo
	pieceInfo     *PieceInfo
	retrievalDeal repo.IRetrievalDealRepo
	retrievalAsk  repo.IRetrievalAskRepo
	rdf           config.RetrievalDealFilter
}

// NewProviderRequestValidator returns a new instance of the ProviderRequestValidator
func NewProviderRequestValidator(
	cfg *config.MarketConfig,
	storageDeals repo.StorageDealRepo,
	retrievalDeal repo.IRetrievalDealRepo,
	retrievalAsk repo.IRetrievalAskRepo,
	pieceInfo *PieceInfo,
	rdf config.RetrievalDealFilter,
) *ProviderRequestValidator {
	return &ProviderRequestValidator{
		cfg:           cfg,
		storageDeals:  storageDeals,
		retrievalDeal: retrievalDeal,
		retrievalAsk:  retrievalAsk,
		pieceInfo:     pieceInfo,
		rdf:           rdf,
	}
}

// ValidatePush validates a push request received from the peer that will send data
func (rv *ProviderRequestValidator) ValidatePush(isRestart bool, _ datatransfer.ChannelID, sender peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) (datatransfer.VoucherResult, error) {
	return nil, errors.New("no pushes accepted")
}

// ValidatePull validates a pull request received from the peer that will receive data
func (rv *ProviderRequestValidator) ValidatePull(isRestart bool, _ datatransfer.ChannelID, receiver peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) (datatransfer.VoucherResult, error) {
	ctx := context.TODO()
	proposal, ok := voucher.(*retrievalmarket.DealProposal)
	var legacyProtocol bool
	if !ok {
		legacyProposal, ok := voucher.(*migrations.DealProposal0)
		if !ok {
			return nil, errors.New("wrong voucher type")
		}
		newProposal := migrations.MigrateDealProposal0To1(*legacyProposal)
		proposal = &newProposal
		legacyProtocol = true
	}
	response, err := rv.validatePull(ctx, isRestart, receiver, proposal, legacyProtocol, baseCid, selector)
	if response == nil {
		return nil, err
	}
	if legacyProtocol {
		downgradedResponse := migrations.DealResponse0{
			Status:      response.Status,
			ID:          response.ID,
			Message:     response.Message,
			PaymentOwed: response.PaymentOwed,
		}
		return &downgradedResponse, err
	}
	return response, err
}

// validatePull is called by the data provider when a new graphsync pull
// request is created. This can be the initial pull request or a new request
// created when the data transfer is restarted (eg after a connection failure).
// By default the graphsync request starts immediately sending data, unless
// validatePull returns ErrPause or the data-transfer has not yet started
// (because the provider is still unsealing the data).
func (rv *ProviderRequestValidator) validatePull(ctx context.Context, isRestart bool, receiver peer.ID, proposal *retrievalmarket.DealProposal, legacyProtocol bool, baseCid cid.Cid, selector ipld.Node) (*retrievalmarket.DealResponse, error) {
	// Check the proposal CID matches
	if proposal.PayloadCID != baseCid {
		return nil, errors.New("incorrect CID for this proposal")
	}

	buf := new(bytes.Buffer)
	err := dagcbor.Encode(selector, buf)
	if err != nil {
		return nil, err
	}
	bytesCompare := allSelectorBytes
	if proposal.SelectorSpecified() {
		bytesCompare = proposal.Selector.Raw
	}
	if !bytes.Equal(buf.Bytes(), bytesCompare) {
		return nil, errors.New("incorrect selector for this proposal")
	}

	// If the validation is for a restart request, return nil, which means
	// the data-transfer should not be explicitly paused or resumed
	if isRestart {
		return nil, nil
	}

	// This is a new graphsync request (not a restart)
	pds := types.ProviderDealState{
		DealProposal:    *proposal,
		Receiver:        receiver,
		LegacyProtocol:  legacyProtocol,
		CurrentInterval: proposal.PaymentInterval,
	}

	// Decide whether to accept the deal
	status, err := rv.acceptDeal(ctx, &pds)

	response := retrievalmarket.DealResponse{
		ID:     proposal.ID,
		Status: status,
	}

	if status == retrievalmarket.DealStatusFundsNeededUnseal {
		response.PaymentOwed = pds.UnsealPrice
	}

	if err != nil {
		response.Message = err.Error()
		return &response, err
	}

	if pds.UnsealPrice.GreaterThan(big.Zero()) {
		pds.Status = retrievalmarket.DealStatusFundsNeededUnseal
		pds.TotalSent = 0

	} else {
		pds.TotalSent = 0
		pds.FundsReceived = abi.NewTokenAmount(0)
	}

	err = rv.retrievalDeal.SaveDeal(ctx, &pds)
	if err != nil {
		response.Message = err.Error()
		return &response, err
	}

	// Pause the data transfer while unsealing the data.
	// The state machine will unpause the transfer when unsealing completes.
	return &response, datatransfer.ErrPause
}

func (rv *ProviderRequestValidator) runDealDecisionLogic(ctx context.Context, deal *types.ProviderDealState) (bool, string, error) {
	if rv.rdf == nil {
		return true, "", nil
	}
	return rv.rdf(ctx, address.Undef, *deal)
}

func (rv *ProviderRequestValidator) acceptDeal(ctx context.Context, deal *types.ProviderDealState) (retrievalmarket.DealStatus, error) {
	minerDeals, err := rv.pieceInfo.GetPieceInfoFromCid(ctx, deal.PayloadCID, deal.PieceCID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return retrievalmarket.DealStatusDealNotFound, err
		}
		return retrievalmarket.DealStatusErrored, err
	}

	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()

	//todo this deal may not match with query ask, no way to get miner id in current protocol
	var ask *types.RetrievalAsk
	for _, minerDeal := range minerDeals {
		minerCfg, err := rv.cfg.MinerProviderConfig(minerDeal.Proposal.Provider, true)
		if err != nil {
			continue
		}
		if minerCfg.RetrievalPaymentAddress.Unwrap().Empty() {
			continue
		}
		deal.SelStorageProposalCid = minerDeal.ProposalCid
		ask, err = rv.retrievalAsk.GetAsk(ctx, minerDeal.Proposal.Provider)
		if err != nil {
			log.Warnf("got %s ask failed: %v", minerDeal.Proposal.Provider, err)
		} else {
			break
		}
	}
	if ask == nil {
		return retrievalmarket.DealStatusErrored, err
	}

	// check that the deal parameters match our required parameters or
	// reject outright
	err = CheckDealParams(ask, deal.PricePerByte, deal.PaymentInterval, deal.PaymentIntervalIncrease, deal.UnsealPrice)
	if err != nil {
		return retrievalmarket.DealStatusRejected, err
	}

	// todo: 检索订单的 `miner` 从哪里来?
	accepted, reason, err := rv.runDealDecisionLogic(ctx, deal)
	if err != nil {
		return retrievalmarket.DealStatusErrored, err
	}
	if !accepted {
		return retrievalmarket.DealStatusRejected, errors.New(reason)
	}

	if deal.UnsealPrice.GreaterThan(big.Zero()) {
		return retrievalmarket.DealStatusFundsNeededUnseal, nil
	}

	return retrievalmarket.DealStatusAccepted, nil
}
