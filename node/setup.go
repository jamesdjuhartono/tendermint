package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dbm "github.com/tendermint/tm-db"

	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/internal/blocksync"
	"github.com/tendermint/tendermint/internal/consensus"
	"github.com/tendermint/tendermint/internal/eventbus"
	"github.com/tendermint/tendermint/internal/evidence"
	"github.com/tendermint/tendermint/internal/mempool"
	"github.com/tendermint/tendermint/internal/p2p"
	"github.com/tendermint/tendermint/internal/p2p/conn"
	"github.com/tendermint/tendermint/internal/p2p/pex"
	"github.com/tendermint/tendermint/internal/proxy"
	sm "github.com/tendermint/tendermint/internal/state"
	"github.com/tendermint/tendermint/internal/state/indexer"
	"github.com/tendermint/tendermint/internal/state/indexer/sink"
	"github.com/tendermint/tendermint/internal/statesync"
	"github.com/tendermint/tendermint/internal/store"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/service"
	tmstrings "github.com/tendermint/tendermint/libs/strings"
	"github.com/tendermint/tendermint/types"
	"github.com/tendermint/tendermint/version"

	_ "net/http/pprof" // nolint: gosec // securely exposed on separate, optional port
)

type closer func() error

func makeCloser(cs []closer) closer {
	return func() error {
		errs := make([]string, 0, len(cs))
		for _, cl := range cs {
			if err := cl(); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) >= 0 {
			return errors.New(strings.Join(errs, "; "))
		}
		return nil
	}
}

func convertCancelCloser(cancel context.CancelFunc) closer {
	return func() error { cancel(); return nil }
}

func combineCloseError(err error, cl closer) error {
	if err == nil {
		return cl()
	}

	clerr := cl()
	if clerr == nil {
		return err
	}

	return fmt.Errorf("error=%q closerError=%q", err.Error(), clerr.Error())
}

func initDBs(
	cfg *config.Config,
	dbProvider config.DBProvider,
) (*store.BlockStore, dbm.DB, closer, error) {

	blockStoreDB, err := dbProvider(&config.DBContext{ID: "blockstore", Config: cfg})
	if err != nil {
		return nil, nil, func() error { return nil }, fmt.Errorf("unable to initialize blockstore: %w", err)
	}
	closers := []closer{}
	blockStore := store.NewBlockStore(blockStoreDB)
	closers = append(closers, blockStoreDB.Close)

	stateDB, err := dbProvider(&config.DBContext{ID: "state", Config: cfg})
	if err != nil {
		return nil, nil, makeCloser(closers), fmt.Errorf("unable to initialize statestore: %w", err)
	}

	closers = append(closers, stateDB.Close)

	return blockStore, stateDB, makeCloser(closers), nil
}

func createAndStartIndexerService(
	ctx context.Context,
	cfg *config.Config,
	dbProvider config.DBProvider,
	eventBus *eventbus.EventBus,
	logger log.Logger,
	chainID string,
	metrics *indexer.Metrics,
) (*indexer.Service, []indexer.EventSink, error) {
	eventSinks, err := sink.EventSinksFromConfig(cfg, dbProvider, chainID)
	if err != nil {
		return nil, nil, err
	}

	indexerService := indexer.NewService(indexer.ServiceArgs{
		Sinks:    eventSinks,
		EventBus: eventBus,
		Logger:   logger.With("module", "txindex"),
		Metrics:  metrics,
	})

	if err := indexerService.Start(ctx); err != nil {
		return nil, nil, err
	}

	return indexerService, eventSinks, nil
}

func logNodeStartupInfo(state sm.State, pubKey crypto.PubKey, logger log.Logger, mode string) {
	// Log the version info.
	logger.Info("Version info",
		"tmVersion", version.TMVersion,
		"block", version.BlockProtocol,
		"p2p", version.P2PProtocol,
		"mode", mode,
	)

	// If the state and software differ in block version, at least log it.
	if state.Version.Consensus.Block != version.BlockProtocol {
		logger.Info("Software and state have different block protocols",
			"software", version.BlockProtocol,
			"state", state.Version.Consensus.Block,
		)
	}

	switch mode {
	case config.ModeFull:
		logger.Info("This node is a fullnode")
	case config.ModeValidator:
		addr := pubKey.Address()
		// Log whether this node is a validator or an observer
		if state.Validators.HasAddress(addr) {
			logger.Info("This node is a validator",
				"addr", addr,
				"pubKey", pubKey.Bytes(),
			)
		} else {
			logger.Info("This node is a validator (NOT in the active validator set)",
				"addr", addr,
				"pubKey", pubKey.Bytes(),
			)
		}
	}
}

func onlyValidatorIsUs(state sm.State, pubKey crypto.PubKey) bool {
	if state.Validators.Size() > 1 {
		return false
	}
	addr, _ := state.Validators.GetByIndex(0)
	return pubKey != nil && bytes.Equal(pubKey.Address(), addr)
}

func createMempoolReactor(
	ctx context.Context,
	cfg *config.Config,
	proxyApp proxy.AppConns,
	state sm.State,
	memplMetrics *mempool.Metrics,
	peerManager *p2p.PeerManager,
	router *p2p.Router,
	logger log.Logger,
) (service.Service, mempool.Mempool, error) {

	logger = logger.With("module", "mempool")

	ch, err := router.OpenChannel(ctx, mempool.GetChannelDescriptor(cfg.Mempool))
	if err != nil {
		return nil, nil, err
	}

	mp := mempool.NewTxMempool(
		logger,
		cfg.Mempool,
		proxyApp.Mempool(),
		state.LastBlockHeight,
		mempool.WithMetrics(memplMetrics),
		mempool.WithPreCheck(sm.TxPreCheck(state)),
		mempool.WithPostCheck(sm.TxPostCheck(state)),
	)

	reactor := mempool.NewReactor(
		logger,
		cfg.Mempool,
		peerManager,
		mp,
		ch,
		peerManager.Subscribe(ctx),
	)

	if cfg.Consensus.WaitForTxs() {
		mp.EnableTxsAvailable()
	}

	return reactor, mp, nil
}

func createEvidenceReactor(
	ctx context.Context,
	cfg *config.Config,
	dbProvider config.DBProvider,
	stateDB dbm.DB,
	blockStore *store.BlockStore,
	peerManager *p2p.PeerManager,
	router *p2p.Router,
	logger log.Logger,
) (*evidence.Reactor, *evidence.Pool, error) {
	evidenceDB, err := dbProvider(&config.DBContext{ID: "evidence", Config: cfg})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to initialize evidence db: %w", err)
	}

	logger = logger.With("module", "evidence")

	evidencePool, err := evidence.NewPool(logger, evidenceDB, sm.NewStore(stateDB), blockStore)
	if err != nil {
		return nil, nil, fmt.Errorf("creating evidence pool: %w", err)
	}

	ch, err := router.OpenChannel(ctx, evidence.GetChannelDescriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("creating evidence channel: %w", err)
	}

	evidenceReactor := evidence.NewReactor(
		logger,
		ch,
		peerManager.Subscribe(ctx),
		evidencePool,
	)

	return evidenceReactor, evidencePool, nil
}

func createBlockchainReactor(
	ctx context.Context,
	logger log.Logger,
	state sm.State,
	blockExec *sm.BlockExecutor,
	blockStore *store.BlockStore,
	csReactor *consensus.Reactor,
	peerManager *p2p.PeerManager,
	router *p2p.Router,
	blockSync bool,
	metrics *consensus.Metrics,
) (service.Service, error) {

	logger = logger.With("module", "blockchain")

	ch, err := router.OpenChannel(ctx, blocksync.GetChannelDescriptor())
	if err != nil {
		return nil, err
	}

	peerUpdates := peerManager.Subscribe(ctx)

	reactor, err := blocksync.NewReactor(
		logger, state.Copy(), blockExec, blockStore, csReactor,
		ch, peerUpdates, blockSync,
		metrics,
	)
	if err != nil {
		return nil, err
	}

	return reactor, nil
}

func createConsensusReactor(
	ctx context.Context,
	cfg *config.Config,
	state sm.State,
	blockExec *sm.BlockExecutor,
	blockStore sm.BlockStore,
	mp mempool.Mempool,
	evidencePool *evidence.Pool,
	privValidator types.PrivValidator,
	csMetrics *consensus.Metrics,
	waitSync bool,
	eventBus *eventbus.EventBus,
	peerManager *p2p.PeerManager,
	router *p2p.Router,
	logger log.Logger,
) (*consensus.Reactor, *consensus.State, error) {
	logger = logger.With("module", "consensus")

	consensusState := consensus.NewState(ctx,
		logger,
		cfg.Consensus,
		state.Copy(),
		blockExec,
		blockStore,
		mp,
		evidencePool,
		consensus.StateMetrics(csMetrics),
	)

	if privValidator != nil && cfg.Mode == config.ModeValidator {
		consensusState.SetPrivValidator(ctx, privValidator)
	}

	csChDesc := consensus.GetChannelDescriptors()
	channels := make(map[p2p.ChannelID]*p2p.Channel, len(csChDesc))
	for idx := range csChDesc {
		chd := csChDesc[idx]
		ch, err := router.OpenChannel(ctx, chd)
		if err != nil {
			return nil, nil, err
		}

		channels[ch.ID] = ch
	}

	reactor := consensus.NewReactor(
		logger,
		consensusState,
		channels[consensus.StateChannel],
		channels[consensus.DataChannel],
		channels[consensus.VoteChannel],
		channels[consensus.VoteSetBitsChannel],
		peerManager.Subscribe(ctx),
		waitSync,
		consensus.ReactorMetrics(csMetrics),
	)

	// Services which will be publishing and/or subscribing for messages (events)
	// consensusReactor will set it on consensusState and blockExecutor.
	reactor.SetEventBus(eventBus)

	return reactor, consensusState, nil
}

func createPeerManager(
	cfg *config.Config,
	dbProvider config.DBProvider,
	nodeID types.NodeID,
) (*p2p.PeerManager, closer, error) {

	privatePeerIDs := make(map[types.NodeID]struct{})
	for _, id := range tmstrings.SplitAndTrimEmpty(cfg.P2P.PrivatePeerIDs, ",", " ") {
		privatePeerIDs[types.NodeID(id)] = struct{}{}
	}

	var maxConns uint16

	switch {
	case cfg.P2P.MaxConnections > 0:
		maxConns = cfg.P2P.MaxConnections
	default:
		maxConns = 64
	}

	options := p2p.PeerManagerOptions{
		MaxConnected:           maxConns,
		MaxConnectedUpgrade:    4,
		MaxPeers:               1000,
		MinRetryTime:           100 * time.Millisecond,
		MaxRetryTime:           8 * time.Hour,
		MaxRetryTimePersistent: 5 * time.Minute,
		RetryTimeJitter:        3 * time.Second,
		PrivatePeers:           privatePeerIDs,
	}

	peers := []p2p.NodeAddress{}
	for _, p := range tmstrings.SplitAndTrimEmpty(cfg.P2P.PersistentPeers, ",", " ") {
		address, err := p2p.ParseNodeAddress(p)
		if err != nil {
			return nil, func() error { return nil }, fmt.Errorf("invalid peer address %q: %w", p, err)
		}

		peers = append(peers, address)
		options.PersistentPeers = append(options.PersistentPeers, address.NodeID)
	}

	for _, p := range tmstrings.SplitAndTrimEmpty(cfg.P2P.BootstrapPeers, ",", " ") {
		address, err := p2p.ParseNodeAddress(p)
		if err != nil {
			return nil, func() error { return nil }, fmt.Errorf("invalid peer address %q: %w", p, err)
		}
		peers = append(peers, address)
	}

	peerDB, err := dbProvider(&config.DBContext{ID: "peerstore", Config: cfg})
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("unable to initialize peer store: %w", err)
	}

	peerManager, err := p2p.NewPeerManager(nodeID, peerDB, options)
	if err != nil {
		return nil, peerDB.Close, fmt.Errorf("failed to create peer manager: %w", err)
	}

	for _, peer := range peers {
		if _, err := peerManager.Add(peer); err != nil {
			return nil, peerDB.Close, fmt.Errorf("failed to add peer %q: %w", peer, err)
		}
	}

	return peerManager, peerDB.Close, nil
}

func createRouter(
	ctx context.Context,
	logger log.Logger,
	p2pMetrics *p2p.Metrics,
	nodeInfo types.NodeInfo,
	nodeKey types.NodeKey,
	peerManager *p2p.PeerManager,
	cfg *config.Config,
	proxyApp proxy.AppConns,
) (*p2p.Router, error) {

	p2pLogger := logger.With("module", "p2p")

	transportConf := conn.DefaultMConnConfig()
	transportConf.FlushThrottle = cfg.P2P.FlushThrottleTimeout
	transportConf.SendRate = cfg.P2P.SendRate
	transportConf.RecvRate = cfg.P2P.RecvRate
	transportConf.MaxPacketMsgPayloadSize = cfg.P2P.MaxPacketMsgPayloadSize
	transport := p2p.NewMConnTransport(
		p2pLogger, transportConf, []*p2p.ChannelDescriptor{},
		p2p.MConnTransportOptions{
			MaxAcceptedConnections: uint32(cfg.P2P.MaxConnections),
		},
	)

	ep, err := p2p.NewEndpoint(nodeKey.ID.AddressString(cfg.P2P.ListenAddress))
	if err != nil {
		return nil, err
	}

	return p2p.NewRouter(
		ctx,
		p2pLogger,
		p2pMetrics,
		nodeInfo,
		nodeKey.PrivKey,
		peerManager,
		[]p2p.Transport{transport},
		[]p2p.Endpoint{ep},
		getRouterConfig(cfg, proxyApp),
	)
}

func createPEXReactor(
	ctx context.Context,
	logger log.Logger,
	peerManager *p2p.PeerManager,
	router *p2p.Router,
) (service.Service, error) {

	channel, err := router.OpenChannel(ctx, pex.ChannelDescriptor())
	if err != nil {
		return nil, err
	}

	return pex.NewReactor(logger, peerManager, channel, peerManager.Subscribe(ctx)), nil
}

func makeNodeInfo(
	cfg *config.Config,
	nodeKey types.NodeKey,
	eventSinks []indexer.EventSink,
	genDoc *types.GenesisDoc,
	state sm.State,
) (types.NodeInfo, error) {

	txIndexerStatus := "off"

	if indexer.IndexingEnabled(eventSinks) {
		txIndexerStatus = "on"
	}

	nodeInfo := types.NodeInfo{
		ProtocolVersion: types.ProtocolVersion{
			P2P:   version.P2PProtocol, // global
			Block: state.Version.Consensus.Block,
			App:   state.Version.Consensus.App,
		},
		NodeID:  nodeKey.ID,
		Network: genDoc.ChainID,
		Version: version.TMVersion,
		Channels: []byte{
			byte(blocksync.BlockSyncChannel),
			byte(consensus.StateChannel),
			byte(consensus.DataChannel),
			byte(consensus.VoteChannel),
			byte(consensus.VoteSetBitsChannel),
			byte(mempool.MempoolChannel),
			byte(evidence.EvidenceChannel),
			byte(statesync.SnapshotChannel),
			byte(statesync.ChunkChannel),
			byte(statesync.LightBlockChannel),
			byte(statesync.ParamsChannel),
		},
		Moniker: cfg.Moniker,
		Other: types.NodeInfoOther{
			TxIndex:    txIndexerStatus,
			RPCAddress: cfg.RPC.ListenAddress,
		},
	}

	if cfg.P2P.PexReactor {
		nodeInfo.Channels = append(nodeInfo.Channels, pex.PexChannel)
	}

	nodeInfo.ListenAddr = cfg.P2P.ExternalAddress
	if nodeInfo.ListenAddr == "" {
		nodeInfo.ListenAddr = cfg.P2P.ListenAddress
	}

	return nodeInfo, nodeInfo.Validate()
}

func makeSeedNodeInfo(
	cfg *config.Config,
	nodeKey types.NodeKey,
	genDoc *types.GenesisDoc,
	state sm.State,
) (types.NodeInfo, error) {
	nodeInfo := types.NodeInfo{
		ProtocolVersion: types.ProtocolVersion{
			P2P:   version.P2PProtocol, // global
			Block: state.Version.Consensus.Block,
			App:   state.Version.Consensus.App,
		},
		NodeID:   nodeKey.ID,
		Network:  genDoc.ChainID,
		Version:  version.TMVersion,
		Channels: []byte{},
		Moniker:  cfg.Moniker,
		Other: types.NodeInfoOther{
			TxIndex:    "off",
			RPCAddress: cfg.RPC.ListenAddress,
		},
	}

	if cfg.P2P.PexReactor {
		nodeInfo.Channels = append(nodeInfo.Channels, pex.PexChannel)
	}

	nodeInfo.ListenAddr = cfg.P2P.ExternalAddress
	if nodeInfo.ListenAddr == "" {
		nodeInfo.ListenAddr = cfg.P2P.ListenAddress
	}

	return nodeInfo, nodeInfo.Validate()
}
