package superchain

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum-optimism/optimism/op-service/sources"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

type SuperchainBackend interface {
	MessageSafety(context.Context, MessageIdentifier, hexutil.Bytes) (MessageSafetyLabel, error)
}

type backend struct {
	log log.Logger
	mu  sync.RWMutex

	l2FinalizedHeadSub  ethereum.Subscription
	l2FinalizedBlockRef *eth.L1BlockRef

	l2PeerNodes map[uint64]client.RPC
}

func NewSuperchainBackend(ctx context.Context, log log.Logger, m metrics.Factory, cfg *SuperchainConfig) (SuperchainBackend, error) {
	log = log.New("module", "superchain")
	backend := backend{log: log, l2PeerNodes: map[uint64]client.RPC{}}

	rpcOpts := []client.RPCOption{client.WithDialBackoff(10)}
	l2Node, err := client.NewRPC(ctx, log, cfg.L2NodeAddr, rpcOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to L2 node: %w", err)
	}

	for chainId, l2NodeAddr := range cfg.PeerL2NodeAddrs {
		l2Node, err := client.NewRPC(ctx, log, l2NodeAddr, rpcOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to Peer L2 node, %d: %w", chainId, err)
		}
		backend.l2PeerNodes[chainId] = l2Node
	}

	/** eth.PollBlockChanges expects an L1BlocksRefSources so we'll use this tooling for now **/
	cacheMetrics := metrics.NewCacheMetrics(m, "superchain", "l2_source_cache", "L2 Source Cache")
	l2ClientConfig := sources.L1ClientConfig{
		L1BlockRefsCacheSize: 10,
		EthClientConfig: sources.EthClientConfig{
			TrustRPC:              true,
			MaxConcurrentRequests: 10,
			MaxRequestsPerBatch:   10,
			TransactionsCacheSize: 10,
			HeadersCacheSize:      10,
			PayloadsCacheSize:     10,
			RPCProviderKind:       sources.RPCKindAny,
			MethodResetDuration:   time.Minute,
		},
	}

	l2Client, err := sources.NewL1Client(l2Node, log, cacheMetrics, &l2ClientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to construct l2 client: %w", err)
	}

	// retrieve the current references before setting up the poll
	finalizedHeadRef, err := l2Client.L1BlockRefByLabel(ctx, eth.Finalized)
	if err != nil {
		return nil, fmt.Errorf("failed to query finalized block ref: %w", err)
	}

	backend.l2FinalizedBlockRef = &finalizedHeadRef
	l2FinalizedHeadSignal := func(ctx context.Context, sig eth.L1BlockRef) {
		backend.mu.Lock()
		backend.l2FinalizedBlockRef = &sig
		backend.mu.Unlock()
	}

	pollInterval, timeout := time.Second*12*32, time.Second*10
	backend.l2FinalizedHeadSub = eth.PollBlockChanges(log, l2Client, l2FinalizedHeadSignal, eth.Finalized, pollInterval, timeout)

	return &backend, nil
}

func (b *backend) MessageSafety(ctx context.Context, id MessageIdentifier, payloadBytes hexutil.Bytes) (MessageSafetyLabel, error) {
	b.log.Info("message safety check", "chain_id", id.ChainId, "block_num", id.BlockNumber, "log_index", id.LogIndex)

	// ChainID Invariant.
	//   TODO: Assumption here that the configured peers exactly maps to the registered dependency set.
	//   When the predeploy is added, this needs to be tied to the dependency set registered on-chain
	//   TODO: Either assume chain id never exceeds uint64 or handle this appropriately
	l2Node, ok := b.l2PeerNodes[id.ChainId.Uint64()]
	if !ok {
		return Invalid, fmt.Errorf("peer with chain id %d is not configured", id.ChainId)
	}

	var logs []types.Log
	var header *types.Header

	// Since eth_getLogs doesn't support specifying the log index, we fetch
	// all the outbox reciepts for this block (TODO: add address filter). The
	// timestamp is grabbed via the block header as getLogs omits this
	blockNumber := hexutil.EncodeBig(id.BlockNumber)
	filterArgs := map[string]interface{}{"fromBlock": blockNumber, "toBlock": blockNumber}
	batchElems := make([]rpc.BatchElem, 2)
	batchElems[0] = rpc.BatchElem{Method: "eth_getBlockByNumber", Args: []interface{}{blockNumber, false}, Result: &header}
	batchElems[1] = rpc.BatchElem{Method: "eth_getLogs", Args: []interface{}{filterArgs}, Result: &logs}
	if err := l2Node.BatchCallContext(ctx, batchElems); err != nil {
		return Invalid, fmt.Errorf("unable to request logs: %w", err)
	}
	if batchElems[0].Error != nil || batchElems[1].Error != nil {
		return Invalid, fmt.Errorf("caught batch rpc failures: getBlockByNumber: %w, getLogs: %w", batchElems[0].Error, batchElems[1].Error)
	}
	if header == nil {
		return Invalid, fmt.Errorf("block %d does not exist", id.BlockNumber)
	}

	// Message Log Integrity
	// 	 -- BlockNumber & ChainID are handled via the RPC connection & inputs

	// TODO: If we filter by address, then this needs to change
	if id.LogIndex >= uint64(len(logs)) {
		return Invalid, fmt.Errorf("invalid log index")
	}

	log := logs[id.LogIndex]
	if id.LogIndex != uint64(log.Index) {
		return Invalid, fmt.Errorf("message log index mismatch")
	}
	if !bytes.Equal(payloadBytes, MessagePayloadBytes(&log)) {
		return Invalid, fmt.Errorf("message payload bytes mismatch")
	}
	if id.Origin != log.Address {
		return Invalid, fmt.Errorf("message origin mismatch")
	}
	if id.Timestamp != header.Time {
		return Invalid, fmt.Errorf("message timestamp mismatch")
	}

	// Message Safety
	//   The block builder & verifier must locally enforce the timestamp invariant. This only
	//   provides fidelity into the safety label of this message relative to its dependencies.

	var finalizedL2Timestamp uint64
	b.mu.RLock()
	finalizedL2Timestamp = b.l2FinalizedBlockRef.Time
	b.mu.RUnlock()

	if id.Timestamp <= finalizedL2Timestamp {
		return Finalized, nil
	}

	// TODO: support for the other safety labels

	return Invalid, nil
}