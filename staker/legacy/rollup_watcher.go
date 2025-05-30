// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package legacystaker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/solgen/go/rollup_legacy_gen"
	"github.com/offchainlabs/nitro/util/headerreader"
)

var rollupInitializedID common.Hash
var nodeCreatedID common.Hash
var challengeCreatedID common.Hash

func init() {
	parsedRollup, err := rollup_legacy_gen.RollupUserLogicMetaData.GetAbi()
	if err != nil {
		panic(err)
	}
	rollupInitializedID = parsedRollup.Events["RollupInitialized"].ID
	nodeCreatedID = parsedRollup.Events["NodeCreated"].ID
	challengeCreatedID = parsedRollup.Events["RollupChallengeStarted"].ID
}

type StakerInfo struct {
	Index            uint64
	LatestStakedNode uint64
	AmountStaked     *big.Int
	CurrentChallenge *uint64
}

type RollupWatcher struct {
	*rollup_legacy_gen.RollupUserLogic
	address             common.Address
	fromBlock           *big.Int
	client              RollupWatcherL1Interface
	baseCallOpts        bind.CallOpts
	unSupportedL3Method atomic.Bool
	supportedL3Method   atomic.Bool
}

type RollupWatcherL1Interface interface {
	bind.ContractBackend
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
}

func NewRollupWatcher(address common.Address, client RollupWatcherL1Interface, callOpts bind.CallOpts) (*RollupWatcher, error) {
	con, err := rollup_legacy_gen.NewRollupUserLogic(address, client)
	if err != nil {
		return nil, err
	}

	return &RollupWatcher{
		address:         address,
		client:          client,
		baseCallOpts:    callOpts,
		RollupUserLogic: con,
	}, nil
}

func (r *RollupWatcher) getCallOpts(ctx context.Context) *bind.CallOpts {
	opts := r.baseCallOpts
	opts.Context = ctx
	return &opts
}

const noNodeErr string = "NO_NODE"

func looksLikeNoNodeError(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), noNodeErr) {
		return true
	}
	var errWithData rpc.DataError
	ok := errors.As(err, &errWithData)
	if !ok {
		return false
	}
	dataString, ok := errWithData.ErrorData().(string)
	if !ok {
		return false
	}
	data := common.FromHex(dataString)
	return bytes.Contains(data, []byte(noNodeErr))
}

func (r *RollupWatcher) getNodeCreationBlock(ctx context.Context, nodeNum uint64) (*big.Int, error) {
	callOpts := r.getCallOpts(ctx)
	if !r.unSupportedL3Method.Load() {
		createdAtBlock, err := r.GetNodeCreationBlockForLogLookup(callOpts, nodeNum)
		if err == nil {
			r.supportedL3Method.Store(true)
			return createdAtBlock, nil
		}
		if headerreader.IsExecutionReverted(err) && !looksLikeNoNodeError(err) {
			if r.supportedL3Method.Load() {
				return nil, fmt.Errorf("getNodeCreationBlockForLogLookup failed despite previously succeeding: %w", err)
			}
			log.Info("getNodeCreationBlockForLogLookup does not seem to exist, falling back on node CreatedAtBlock field", "err", err)
			r.unSupportedL3Method.Store(true)
		} else {
			return nil, err
		}
	}
	node, err := r.GetNode(callOpts, nodeNum)
	if err != nil {
		return nil, err
	}
	createdAtBlock := new(big.Int).SetUint64(node.CreatedAtBlock)
	return createdAtBlock, nil
}

func (r *RollupWatcher) Initialize(ctx context.Context) error {
	var err error
	r.fromBlock, err = r.getNodeCreationBlock(ctx, 0)
	return err
}

func (r *RollupWatcher) Client() RollupWatcherL1Interface {
	return r.client
}

func (r *RollupWatcher) LookupCreation(ctx context.Context) (*rollup_legacy_gen.RollupUserLogicRollupInitialized, error) {
	var query = ethereum.FilterQuery{
		FromBlock: r.fromBlock,
		ToBlock:   r.fromBlock,
		Addresses: []common.Address{r.address},
		Topics:    [][]common.Hash{{rollupInitializedID}},
	}
	logs, err := r.client.FilterLogs(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(logs) == 0 {
		return nil, errors.New("rollup not created")
	}
	if len(logs) > 1 {
		return nil, errors.New("rollup created multiple times")
	}
	ev, err := r.ParseRollupInitialized(logs[0])
	return ev, err
}

func (r *RollupWatcher) LookupNode(ctx context.Context, number uint64) (*NodeInfo, error) {
	createdAtBlock, err := r.getNodeCreationBlock(ctx, number)
	if err != nil {
		return nil, err
	}
	var numberAsHash common.Hash
	binary.BigEndian.PutUint64(numberAsHash[(32-8):], number)
	var query = ethereum.FilterQuery{
		FromBlock: createdAtBlock,
		ToBlock:   createdAtBlock,
		Addresses: []common.Address{r.address},
		Topics:    [][]common.Hash{{nodeCreatedID}, {numberAsHash}},
	}
	logs, err := r.client.FilterLogs(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(logs) == 0 {
		return nil, fmt.Errorf("couldn't find requested node %v", number)
	}
	if len(logs) > 1 {
		return nil, fmt.Errorf("found multiple instances of requested node %v", number)
	}
	ethLog := logs[0]
	parsedLog, err := r.ParseNodeCreated(ethLog)
	if err != nil {
		return nil, err
	}
	l1BlockProposed, err := arbutil.CorrespondingL1BlockNumber(ctx, r.client, ethLog.BlockNumber)
	if err != nil {
		return nil, err
	}
	return &NodeInfo{
		NodeNum:                  parsedLog.NodeNum,
		L1BlockProposed:          l1BlockProposed,
		ParentChainBlockProposed: ethLog.BlockNumber,
		Assertion:                NewAssertionFromLegacySolidity(parsedLog.Assertion),
		InboxMaxCount:            parsedLog.InboxMaxCount,
		AfterInboxBatchAcc:       parsedLog.AfterInboxBatchAcc,
		NodeHash:                 parsedLog.NodeHash,
		WasmModuleRoot:           parsedLog.WasmModuleRoot,
	}, nil
}

func (r *RollupWatcher) LookupNodeChildren(ctx context.Context, nodeNum uint64, logQueryRangeSize uint64, nodeHash common.Hash) ([]*NodeInfo, error) {
	node, err := r.RollupUserLogic.GetNode(r.getCallOpts(ctx), nodeNum)
	if err != nil {
		return nil, err
	}
	if node.LatestChildNumber == 0 {
		return nil, nil
	}
	if node.NodeHash != nodeHash {
		return nil, fmt.Errorf("got unexpected node hash %v looking for node number %v with expected hash %v (reorg?)", node.NodeHash, nodeNum, nodeHash)
	}
	var query = ethereum.FilterQuery{
		Addresses: []common.Address{r.address},
		Topics:    [][]common.Hash{{nodeCreatedID}, nil, {nodeHash}},
	}
	fromBlock, err := r.getNodeCreationBlock(ctx, nodeNum)
	if err != nil {
		return nil, err
	}
	toBlock, err := r.getNodeCreationBlock(ctx, node.LatestChildNumber)
	if err != nil {
		return nil, err
	}
	var logs []types.Log
	// break down the query to avoid eth_getLogs query limit
	for toBlock.Cmp(fromBlock) > 0 {
		query.FromBlock = fromBlock
		if logQueryRangeSize == 0 {
			query.ToBlock = toBlock
		} else {
			query.ToBlock = new(big.Int).Add(fromBlock, new(big.Int).SetUint64(logQueryRangeSize))
		}
		if query.ToBlock.Cmp(toBlock) > 0 {
			query.ToBlock = toBlock
		}
		segment, err := r.client.FilterLogs(ctx, query)
		if err != nil {
			return nil, err
		}
		logs = append(logs, segment...)
		fromBlock = new(big.Int).Add(query.ToBlock, big.NewInt(1))
	}
	infos := make([]*NodeInfo, 0, len(logs))
	lastHash := nodeHash
	for i, ethLog := range logs {
		parsedLog, err := r.ParseNodeCreated(ethLog)
		if err != nil {
			return nil, err
		}
		lastHashIsSibling := [1]byte{0}
		if i > 0 {
			lastHashIsSibling[0] = 1
		}
		lastHash = crypto.Keccak256Hash(lastHashIsSibling[:], lastHash[:], parsedLog.ExecutionHash[:], parsedLog.AfterInboxBatchAcc[:], parsedLog.WasmModuleRoot[:])
		l1BlockProposed, err := arbutil.CorrespondingL1BlockNumber(ctx, r.client, ethLog.BlockNumber)
		if err != nil {
			return nil, err
		}
		infos = append(infos, &NodeInfo{
			NodeNum:                  parsedLog.NodeNum,
			L1BlockProposed:          l1BlockProposed,
			ParentChainBlockProposed: ethLog.BlockNumber,
			Assertion:                NewAssertionFromLegacySolidity(parsedLog.Assertion),
			InboxMaxCount:            parsedLog.InboxMaxCount,
			AfterInboxBatchAcc:       parsedLog.AfterInboxBatchAcc,
			NodeHash:                 lastHash,
			WasmModuleRoot:           parsedLog.WasmModuleRoot,
		})
	}
	return infos, nil
}

func (r *RollupWatcher) LatestConfirmedCreationBlock(ctx context.Context) (uint64, error) {
	latestConfirmed, err := r.LatestConfirmed(r.getCallOpts(ctx))
	if err != nil {
		return 0, err
	}
	creation, err := r.getNodeCreationBlock(ctx, latestConfirmed)
	if err != nil {
		return 0, err
	}
	if !creation.IsUint64() {
		return 0, fmt.Errorf("node %v creation block %v is not a uint64", latestConfirmed, creation)
	}
	return creation.Uint64(), nil
}

func (r *RollupWatcher) LookupChallengedNode(ctx context.Context, address common.Address) (uint64, error) {
	// TODO: This function is currently unused

	// Assuming this function is only used to find information about an active challenge, it
	// must be a challenge over an unconfirmed node and thus must have been created after the
	// latest confirmed node was created
	latestConfirmedCreated, err := r.LatestConfirmedCreationBlock(ctx)
	if err != nil {
		return 0, err
	}

	addressQuery := common.Hash{}
	copy(addressQuery[12:], address.Bytes())

	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(latestConfirmedCreated),
		ToBlock:   nil,
		Addresses: []common.Address{r.address},
		Topics:    [][]common.Hash{{challengeCreatedID}, {addressQuery}},
	}
	logs, err := r.client.FilterLogs(ctx, query)
	if err != nil {
		return 0, err
	}

	if len(logs) == 0 {
		return 0, errors.New("no matching challenge")
	}

	if len(logs) > 1 {
		return 0, errors.New("too many matching challenges")
	}

	challenge, err := r.ParseRollupChallengeStarted(logs[0])
	if err != nil {
		return 0, err
	}

	return challenge.ChallengedNode, nil
}

func (r *RollupWatcher) StakerInfo(ctx context.Context, staker common.Address) (*StakerInfo, error) {
	info, err := r.StakerMap(r.getCallOpts(ctx), staker)
	if err != nil {
		return nil, err
	}
	if !info.IsStaked {
		return nil, nil
	}
	stakerInfo := &StakerInfo{
		Index:            info.Index,
		LatestStakedNode: info.LatestStakedNode,
		AmountStaked:     info.AmountStaked,
	}
	if info.CurrentChallenge != 0 {
		chal := info.CurrentChallenge
		stakerInfo.CurrentChallenge = &chal
	}
	return stakerInfo, nil
}
