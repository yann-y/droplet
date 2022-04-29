package main

import (
	"context"

	"github.com/filecoin-project/venus-auth/cmd/jwtclient"
	"github.com/filecoin-project/venus-market/v2/api/clients"
	"github.com/filecoin-project/venus-market/v2/api/impl"
	cli2 "github.com/filecoin-project/venus-market/v2/cli"
	"github.com/filecoin-project/venus-market/v2/config"
	"github.com/filecoin-project/venus-market/v2/dagstore"
	"github.com/filecoin-project/venus-market/v2/fundmgr"
	"github.com/filecoin-project/venus-market/v2/minermgr"
	"github.com/filecoin-project/venus-market/v2/models"
	"github.com/filecoin-project/venus-market/v2/network"
	"github.com/filecoin-project/venus-market/v2/paychmgr"
	"github.com/filecoin-project/venus-market/v2/piecestorage"
	"github.com/filecoin-project/venus-market/v2/retrievalprovider"
	"github.com/filecoin-project/venus-market/v2/rpc"
	"github.com/filecoin-project/venus-market/v2/storageprovider"
	types2 "github.com/filecoin-project/venus-market/v2/types"
	"github.com/filecoin-project/venus-market/v2/utils"
	marketapi "github.com/filecoin-project/venus/venus-shared/api/market"
	"github.com/filecoin-project/venus/venus-shared/api/permission"
	"github.com/gorilla/mux"
	"github.com/ipfs-force-community/venus-common-utils/builder"
	"github.com/ipfs-force-community/venus-common-utils/journal"
	"github.com/ipfs-force-community/venus-common-utils/metrics"
	metrics2 "github.com/ipfs/go-metrics-interface"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"golang.org/x/xerrors"
)

var poolRunCmd = &cli.Command{
	Name:      "pool-run",
	Usage:     "Run the market daemon in pool mode",
	ArgsUsage: "[minerAddress]",
	Flags: []cli.Flag{
		NodeUrlFlag,
		NodeTokenFlag,
		AuthUrlFlag,
		AuthTokeFlag,
		MessagerUrlFlag,
		MessagerTokenFlag,
		HidenSignerTypeFlag,
		GatewayUrlFlag,
		GatewayTokenFlag,
		MysqlDsnFlag,
		MinerListFlag,
		PaymentAddressFlag,
	},
	Action: poolDaemon,
}

func poolDaemon(cctx *cli.Context) error {
	utils.SetupLogLevels()
	ctx := cctx.Context
	if !cctx.IsSet(HidenSignerTypeFlag.Name) {
		if err := cctx.Set(HidenSignerTypeFlag.Name, "gateway"); err != nil {
			return xerrors.Errorf("set %s with gateway failed %v", HidenSignerTypeFlag.Name, err)
		}
	}
	cfg, err := prepare(cctx)
	if err != nil {
		return err
	}

	// venus-auth is must in 'pool' mode
	if len(cfg.AuthNode.Url) == 0 {
		return xerrors.Errorf("auth-url is required in 'pool' mode")
	}

	// 'NewAuthClient' never returns an error, no needs to check
	authClient, _ := jwtclient.NewAuthClient(cfg.AuthNode.Url)

	resAPI := &impl.MarketNodeImpl{}
	shutdownChan := make(chan struct{})
	_, err = builder.New(ctx,
		//defaults
		builder.Override(new(*jwtclient.AuthClient), authClient),
		builder.Override(new(journal.DisabledEvents), journal.EnvDisabledEvents),
		builder.Override(new(journal.Journal), func(lc fx.Lifecycle, home config.IHome, disabled journal.DisabledEvents) (journal.Journal, error) {
			return journal.OpenFilesystemJournal(lc, home.MustHomePath(), "venus-market", disabled)
		}),

		builder.Override(new(metrics.MetricsCtx), func() context.Context {
			return metrics2.CtxScope(context.Background(), "venus-market")
		}),
		builder.Override(new(types2.ShutdownChan), shutdownChan),
		//config
		config.ConfigServerOpts(cfg),

		// miner manager
		minermgr.MinerMgrOpts(cfg),

		//clients
		clients.ClientsOpts(true, "pool", &cfg.Messager, &cfg.Signer),
		models.DBOptions(true, &cfg.Mysql),
		network.NetworkOpts(true, cfg.SimultaneousTransfersForRetrieval, cfg.SimultaneousTransfersForStoragePerClient, cfg.SimultaneousTransfersForStorage),
		piecestorage.PieceStorageOpts(cfg),
		fundmgr.FundMgrOpts,
		dagstore.DagstoreOpts,
		paychmgr.PaychOpts,
		// Markets
		storageprovider.StorageProviderOpts(cfg),
		retrievalprovider.RetrievalProviderOpts(cfg),

		func(s *builder.Settings) error {
			s.Invokes[ExtractApiKey] = builder.InvokeOption{
				Priority: 10,
				Option:   fx.Populate(resAPI),
			}
			return nil
		},
	)
	if err != nil {
		return xerrors.Errorf("initializing node: %w", err)
	}
	finishCh := utils.MonitorShutdown(shutdownChan)

	mux := mux.NewRouter()
	if err = mux.Handle("/resource", rpc.NewPieceStorageServer(resAPI.PieceStorageMgr)).GetError(); err != nil {
		return xerrors.Errorf("handle 'resource' failed: %w", err)
	}

	var fullAPI marketapi.IMarketStruct
	permission.PermissionProxy(marketapi.IMarket(resAPI), &fullAPI)

	return rpc.ServeRPC(ctx, cfg, &cfg.API, mux, 1000, cli2.API_NAMESPACE_VENUS_MARKET, authClient, &fullAPI, finishCh)
}
