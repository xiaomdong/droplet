package paychmgr

import (
	"context"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/venus/venus-shared/types"
)

type BestSpendableAPI interface {
	PaychVoucherList(context.Context, address.Address) ([]*types.SignedVoucher, error)
	PaychVoucherCheckSpendable(context.Context, address.Address, *types.SignedVoucher, []byte, []byte) (bool, error)
}

// BestSpendableByLane return spendable  voucher in channel address
func BestSpendableByLane(ctx context.Context, api BestSpendableAPI, ch address.Address) (map[uint64]*types.SignedVoucher, error) {
	vouchers, err := api.PaychVoucherList(ctx, ch)
	if err != nil {
		return nil, err
	}

	bestByLane := make(map[uint64]*types.SignedVoucher)
	for _, voucher := range vouchers {
		spendable, err := api.PaychVoucherCheckSpendable(ctx, ch, voucher, nil, nil)
		if err != nil {
			return nil, err
		}
		if spendable {
			if bestByLane[voucher.Lane] == nil || voucher.Amount.GreaterThan(bestByLane[voucher.Lane].Amount) {
				bestByLane[voucher.Lane] = voucher
			}
		}
	}
	return bestByLane, nil
}
