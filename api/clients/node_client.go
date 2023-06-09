package clients

import (
	"context"

	"go.uber.org/fx"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc"

	"github.com/ipfs-force-community/droplet/v2/config"
	"github.com/ipfs-force-community/droplet/v2/utils"

	"github.com/filecoin-project/venus/venus-shared/api"
	v1api "github.com/filecoin-project/venus/venus-shared/api/chain/v1"
	"github.com/filecoin-project/venus/venus-shared/types"

	"github.com/ipfs-force-community/metrics"
)

func NodeClient(mctx metrics.MetricsCtx, lc fx.Lifecycle, nodeCfg *config.Node) (v1api.FullNode, error) {
	fullNode := v1api.FullNodeStruct{}

	aInfo := api.NewAPIInfo(nodeCfg.Url, nodeCfg.Token)
	addr, err := aInfo.DialArgs("v1")
	if err != nil {
		return nil, err
	}

	closer, err := jsonrpc.NewMergeClient(mctx, addr, "Filecoin", utils.GetInternalStructs(&fullNode), aInfo.AuthHeader())
	if err != nil {
		return nil, err
	}

	netName, err := fullNode.StateNetworkName(mctx)
	if err != nil {
		return nil, err
	}
	if netName == types.NetworkNameMain {
		address.CurrentNetwork = address.Mainnet
	}

	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			closer()
			return nil
		},
	})
	return &fullNode, err
}
