// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package raft

import (
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eapache/channels"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"strings"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Current state information for building the next block
type work struct {
	config       *params.ChainConfig
	publicState  *state.StateDB
	privateState *state.StateDB
	Block        *types.Block
	header       *types.Header
}

type minter struct {
	config           *params.ChainConfig
	mu               sync.Mutex
	mux              *event.TypeMux
	eth              miner.Backend
	chain            *core.BlockChain
	chainDb          ethdb.Database
	coinbase         common.Address
	minting          int32 // Atomic status counter
	shouldMine       *channels.RingChannel
	blockTime        time.Duration
	speculativeChain *speculativeChain

	invalidRaftOrderingChan chan InvalidRaftOrdering
	chainHeadChan           chan core.ChainHeadEvent
	chainHeadSub            event.Subscription
	txPreChan               chan core.TxPreEvent
	txPreSub                event.Subscription
}

func newMinter(config *params.ChainConfig, eth *RaftService, blockTime time.Duration) *minter {
	minter := &minter{
		config:           config,
		eth:              eth,
		mux:              eth.EventMux(),
		chainDb:          eth.ChainDb(),
		chain:            eth.BlockChain(),
		shouldMine:       channels.NewRingChannel(1),
		blockTime:        blockTime,
		speculativeChain: newSpeculativeChain(),

		invalidRaftOrderingChan: make(chan InvalidRaftOrdering, 1),
		chainHeadChan:           make(chan core.ChainHeadEvent, 1),
		txPreChan:               make(chan core.TxPreEvent, 4096),
	}

	minter.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(minter.chainHeadChan)
	minter.txPreSub = eth.TxPool().SubscribeTxPreEvent(minter.txPreChan)

	minter.speculativeChain.clear(minter.chain.CurrentBlock())

	go minter.eventLoop()
	go minter.mintingLoop()

	return minter
}

func (minter *minter) start() {
	atomic.StoreInt32(&minter.minting, 1)
	minter.requestMinting()
}

func (minter *minter) stop() {
	minter.mu.Lock()
	defer minter.mu.Unlock()

	minter.speculativeChain.clear(minter.chain.CurrentBlock())
	atomic.StoreInt32(&minter.minting, 0)
}

// Notify the minting loop that minting should occur, if it's not already been
// requested. Due to the use of a RingChannel, this function is idempotent if
// called multiple times before the minting occurs.
func (minter *minter) requestMinting() {
	minter.shouldMine.In() <- struct{}{}
}

type AddressTxes map[common.Address]types.Transactions

func (minter *minter) updateSpeculativeChainPerNewHead(newHeadBlock *types.Block) {
	minter.mu.Lock()
	defer minter.mu.Unlock()

	minter.speculativeChain.accept(newHeadBlock)
}

func (minter *minter) updateSpeculativeChainPerInvalidOrdering(headBlock *types.Block, invalidBlock *types.Block) {
	invalidHash := invalidBlock.Hash()

	log.Info("Handling InvalidRaftOrdering", "invalid block", invalidHash, "current head", headBlock.Hash())

	minter.mu.Lock()
	defer minter.mu.Unlock()

	// 1. if the block is not in our db, exit. someone else mined this.
	if !minter.chain.HasBlock(invalidHash, invalidBlock.NumberU64()) {
		log.Info("Someone else mined invalid block; ignoring", "block", invalidHash)

		return
	}

	minter.speculativeChain.unwindFrom(invalidHash, headBlock)
}

func (minter *minter) eventLoop() {
	defer minter.chainHeadSub.Unsubscribe()
	defer minter.txPreSub.Unsubscribe()

	for {
		select {
		case ev := <-minter.chainHeadChan:
			newHeadBlock := ev.Block

			if atomic.LoadInt32(&minter.minting) == 1 {
				minter.updateSpeculativeChainPerNewHead(newHeadBlock)

				//
				// TODO(bts): not sure if this is the place, but we're going to
				// want to put an upper limit on our speculative mining chain
				// length.
				//

				minter.requestMinting()
			} else {
				minter.mu.Lock()
				minter.speculativeChain.setHead(newHeadBlock)
				minter.mu.Unlock()
			}

		case <-minter.txPreChan:
			if atomic.LoadInt32(&minter.minting) == 1 {
				minter.requestMinting()
			}

		case ev := <-minter.invalidRaftOrderingChan:
			headBlock := ev.headBlock
			invalidBlock := ev.invalidBlock

			minter.updateSpeculativeChainPerInvalidOrdering(headBlock, invalidBlock)

		// system stopped
		case <-minter.chainHeadSub.Err():
			return
		case <-minter.txPreSub.Err():
			return
		}
	}
}

// Returns a wrapper around no-arg func `f` which can be called without limit
// and returns immediately: this will call the underlying func `f` at most once
// every `rate`. If this function is called more than once before the underlying
// `f` is invoked (per this rate limiting), `f` will only be called *once*.
//
// TODO(joel): this has a small bug in that you can't call it *immediately* when
// first allocated.
func throttle(rate time.Duration, f func()) func() {
	request := channels.NewRingChannel(1)

	// every tick, block waiting for another request. then serve it immediately
	go func() {
		ticker := time.NewTicker(rate)
		defer ticker.Stop()

		for range ticker.C {
			<-request.Out()
			go f()
		}
	}()

	return func() {
		request.In() <- struct{}{}
	}
}

// This function spins continuously, blocking until a block should be created
// (via requestMinting()). This is throttled by `minter.blockTime`:
//
//   1. A block is guaranteed to be minted within `blockTime` of being
//      requested.
//   2. We never mint a block more frequently than `blockTime`.
func (minter *minter) mintingLoop() {
	throttledMintNewBlock := throttle(minter.blockTime, func() {
		if atomic.LoadInt32(&minter.minting) == 1 {
			minter.mintNewBlock()
		}
	})

	for range minter.shouldMine.Out() {
		throttledMintNewBlock()
	}
}

func generateNanoTimestamp(parent *types.Block) (tstamp int64) {
	parentTime := parent.Time().Int64()
	tstamp = time.Now().UnixNano()

	if parentTime >= tstamp {
		// Each successive block needs to be after its predecessor.
		tstamp = parentTime + 1
	}

	return
}

// Assumes mu is held.
func (minter *minter) createWork() *work {
	parent := minter.speculativeChain.head
	parentNumber := parent.Number()
	tstamp := generateNanoTimestamp(parent)

	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     parentNumber.Add(parentNumber, common.Big1),
		Difficulty: ethash.CalcDifficulty(minter.config, uint64(tstamp), parent.Header()),
		GasLimit:   core.CalcGasLimit(parent),
		GasUsed:    new(big.Int),
		Coinbase:   minter.coinbase,
		Time:       big.NewInt(tstamp),
	}

	publicState, privateState, err := minter.chain.StateAt(parent.Root())
	if err != nil {
		panic(fmt.Sprint("failed to get parent state: ", err))
	}

	return &work{
		config:       minter.config,
		publicState:  publicState,
		privateState: privateState,
		header:       header,
	}
}

func (minter *minter) getTransactions() *types.TransactionsByPriceAndNonce {
	allAddrTxes, err := minter.eth.TxPool().Pending()
	if err != nil { // TODO: handle
		panic(err)
	}
	addrTxes := minter.speculativeChain.withoutProposedTxes(allAddrTxes)
	signer := types.MakeSigner(minter.chain.Config(), minter.chain.CurrentBlock().Number())
	return types.NewTransactionsByPriceAndNonce(signer, addrTxes)
}

// Sends-off events asynchronously.
func (minter *minter) firePendingBlockEvents(logs []*types.Log) {
	// Copy logs before we mutate them, adding a block hash.
	copiedLogs := make([]*types.Log, len(logs))
	for i, l := range logs {
		copiedLogs[i] = new(types.Log)
		*copiedLogs[i] = *l
	}

	go func() {
		minter.mux.Post(core.PendingLogsEvent{Logs: copiedLogs})
		minter.mux.Post(core.PendingStateEvent{})
	}()
}

func (minter *minter) mintNewBlock() {
	minter.mu.Lock()
	defer minter.mu.Unlock()
	instantiateStakecheck()
	work := minter.createWork()
	transactions := minter.getTransactions()

	committedTxes, publicReceipts, privateReceipts, logs := work.commitTransactions(transactions, minter.chain)
	txCount := len(committedTxes)

	if txCount == 0 {
		log.Info("Not minting a new block since there are no pending transactions")
		return
	}

	minter.firePendingBlockEvents(logs)

	header := work.header

	// commit state root after all state transitions.
	ethash.AccumulateRewards(minter.chain.Config(), work.publicState, header, nil)
	header.Root = work.publicState.IntermediateRoot(minter.chain.Config().IsEIP158(work.header.Number))

	// NOTE: < QuorumChain creates a signature here and puts it in header.Extra. >

	allReceipts := append(publicReceipts, privateReceipts...)
	header.Bloom = types.CreateBloom(allReceipts)

	// update block hash since it is now available, but was not when the
	// receipt/log of individual transactions were created:
	headerHash := header.Hash()
	for _, l := range logs {
		l.BlockHash = headerHash
	}

	block := types.NewBlock(header, committedTxes, nil, publicReceipts)

	log.Info("Generated next block", "block num", block.Number(), "num txes", txCount)

	deleteEmptyObjects := minter.chain.Config().IsEIP158(block.Number())
	if _, err := work.publicState.CommitTo(minter.chainDb, deleteEmptyObjects); err != nil {
		panic(fmt.Sprint("error committing public state: ", err))
	}
	if _, privStateErr := work.privateState.CommitTo(minter.chainDb, deleteEmptyObjects); privStateErr != nil {
		panic(fmt.Sprint("error committing private state: ", privStateErr))
	}

	minter.speculativeChain.extend(block)

	minter.mux.Post(core.NewMinedBlockEvent{Block: block})

	elapsed := time.Since(time.Unix(0, header.Time.Int64()))
	log.Info("🔨  Mined block", "number", block.Number(), "hash", fmt.Sprintf("%x", block.Hash().Bytes()[:4]), "elapsed", elapsed)
}

func (env *work) commitTransactions(txes *types.TransactionsByPriceAndNonce, bc *core.BlockChain) (types.Transactions, types.Receipts, types.Receipts, []*types.Log) {
	var allLogs []*types.Log
	var committedTxes types.Transactions
	var publicReceipts types.Receipts
	var privateReceipts types.Receipts

	gp := new(core.GasPool).AddGas(env.header.GasLimit)
	txCount := 0

	for {
		tx := txes.Peek()
		if tx == nil {
			break
		}

		env.publicState.Prepare(tx.Hash(), common.Hash{}, txCount)

		publicReceipt, privateReceipt, err := env.commitTransaction(tx, bc, gp)
		switch {
		case err != nil:
			log.Info("TX failed, will be removed", "hash", tx.Hash(), "err", err)
			txes.Pop() // skip rest of txes from this account
		default:
			txCount++
			committedTxes = append(committedTxes, tx)

			publicReceipts = append(publicReceipts, publicReceipt)
			allLogs = append(allLogs, publicReceipt.Logs...)

			if privateReceipt != nil {
				privateReceipts = append(privateReceipts, privateReceipt)
				allLogs = append(allLogs, privateReceipt.Logs...)
			}

			txes.Shift()
		}
	}

	return committedTxes, publicReceipts, privateReceipts, allLogs
}

func (env *work) commitTransaction(tx *types.Transaction, bc *core.BlockChain, gp *core.GasPool) (*types.Receipt, *types.Receipt, error) {
	publicSnapshot := env.publicState.Snapshot()
	privateSnapshot := env.privateState.Snapshot()

	var author *common.Address
	var vmConf vm.Config
	publicReceipt, privateReceipt, _, err := core.ApplyTransaction(env.config, bc, author, gp, env.publicState, env.privateState, env.header, tx, env.header.GasUsed, vmConf)
	if err != nil {
		env.publicState.RevertToSnapshot(publicSnapshot)
		env.privateState.RevertToSnapshot(privateSnapshot)

		return nil, nil, err
	}

	return publicReceipt, privateReceipt, nil
}

// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

// StakecheckABI is the input ABI used to generate the binding from.
const StakecheckABI = "[{\"constant\":false,\"inputs\":[{\"name\":\"_spender\",\"type\":\"address\"},{\"name\":\"_value\",\"type\":\"uint256\"}],\"name\":\"approve\",\"outputs\":[{\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"totalSupply\",\"outputs\":[{\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"name\":\"_from\",\"type\":\"address\"},{\"name\":\"_to\",\"type\":\"address\"},{\"name\":\"_value\",\"type\":\"uint256\"}],\"name\":\"transferFrom\",\"outputs\":[{\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"name\":\"_value\",\"type\":\"uint256\"},{\"name\":\"_addr\",\"type\":\"address\"}],\"name\":\"addStake\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"name\":\"_value\",\"type\":\"uint256\"},{\"name\":\"_addr\",\"type\":\"address\"}],\"name\":\"releaseStake\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"name\":\"_spender\",\"type\":\"address\"},{\"name\":\"_subtractedValue\",\"type\":\"uint256\"}],\"name\":\"decreaseApproval\",\"outputs\":[{\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"name\":\"_owner\",\"type\":\"address\"}],\"name\":\"balanceOf\",\"outputs\":[{\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"owner\",\"outputs\":[{\"name\":\"\",\"type\":\"address\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"name\":\"_addr\",\"type\":\"address\"}],\"name\":\"checkStake\",\"outputs\":[{\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"name\":\"_to\",\"type\":\"address\"},{\"name\":\"_value\",\"type\":\"uint256\"}],\"name\":\"transfer\",\"outputs\":[{\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"name\":\"_spender\",\"type\":\"address\"},{\"name\":\"_addedValue\",\"type\":\"uint256\"}],\"name\":\"increaseApproval\",\"outputs\":[{\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"name\":\"_owner\",\"type\":\"address\"},{\"name\":\"_spender\",\"type\":\"address\"}],\"name\":\"allowance\",\"outputs\":[{\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"name\":\"newOwner\",\"type\":\"address\"}],\"name\":\"transferOwnership\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"inputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"constructor\"},{\"anonymous\":false,\"inputs\":[{\"indexed\":false,\"name\":\"_masternodeOwner\",\"type\":\"address\"},{\"indexed\":false,\"name\":\"_stakeAmount\",\"type\":\"uint256\"}],\"name\":\"StakeAdded\",\"type\":\"event\"},{\"anonymous\":false,\"inputs\":[{\"indexed\":false,\"name\":\"_masternodeOwner\",\"type\":\"address\"},{\"indexed\":false,\"name\":\"_stakeAmount\",\"type\":\"uint256\"}],\"name\":\"StakeReleased\",\"type\":\"event\"},{\"anonymous\":false,\"inputs\":[{\"indexed\":true,\"name\":\"owner\",\"type\":\"address\"},{\"indexed\":true,\"name\":\"spender\",\"type\":\"address\"},{\"indexed\":false,\"name\":\"value\",\"type\":\"uint256\"}],\"name\":\"Approval\",\"type\":\"event\"},{\"anonymous\":false,\"inputs\":[{\"indexed\":true,\"name\":\"from\",\"type\":\"address\"},{\"indexed\":true,\"name\":\"to\",\"type\":\"address\"},{\"indexed\":false,\"name\":\"value\",\"type\":\"uint256\"}],\"name\":\"Transfer\",\"type\":\"event\"},{\"anonymous\":false,\"inputs\":[{\"indexed\":true,\"name\":\"previousOwner\",\"type\":\"address\"},{\"indexed\":true,\"name\":\"newOwner\",\"type\":\"address\"}],\"name\":\"OwnershipTransferred\",\"type\":\"event\"}]"

// Stakecheck is an auto generated Go binding around an Ethereum contract.
type Stakecheck struct {
	StakecheckCaller     // Read-only binding to the contract
	StakecheckTransactor // Write-only binding to the contract
}

// StakecheckCaller is an auto generated read-only Go binding around an Ethereum contract.
type StakecheckCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// StakecheckTransactor is an auto generated write-only Go binding around an Ethereum contract.
type StakecheckTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// StakecheckSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type StakecheckSession struct {
	Contract     *Stakecheck       // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// StakecheckCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type StakecheckCallerSession struct {
	Contract *StakecheckCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts     // Call options to use throughout this session
}

// StakecheckTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type StakecheckTransactorSession struct {
	Contract     *StakecheckTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts     // Transaction auth options to use throughout this session
}

// StakecheckRaw is an auto generated low-level Go binding around an Ethereum contract.
type StakecheckRaw struct {
	Contract *Stakecheck // Generic contract binding to access the raw methods on
}

// StakecheckCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type StakecheckCallerRaw struct {
	Contract *StakecheckCaller // Generic read-only contract binding to access the raw methods on
}

// StakecheckTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type StakecheckTransactorRaw struct {
	Contract *StakecheckTransactor // Generic write-only contract binding to access the raw methods on
}

// NewStakecheck creates a new instance of Stakecheck, bound to a specific deployed contract.
func NewStakecheck(address common.Address, backend bind.ContractBackend) (*Stakecheck, error) {
	contract, err := bindStakecheck(address, backend, backend)
	if err != nil {
		return nil, err
	}
	return &Stakecheck{StakecheckCaller: StakecheckCaller{contract: contract}, StakecheckTransactor: StakecheckTransactor{contract: contract}}, nil
}

// NewStakecheckCaller creates a new read-only instance of Stakecheck, bound to a specific deployed contract.
func NewStakecheckCaller(address common.Address, caller bind.ContractCaller) (*StakecheckCaller, error) {
	contract, err := bindStakecheck(address, caller, nil)
	if err != nil {
		return nil, err
	}
	return &StakecheckCaller{contract: contract}, nil
}

// NewStakecheckTransactor creates a new write-only instance of Stakecheck, bound to a specific deployed contract.
func NewStakecheckTransactor(address common.Address, transactor bind.ContractTransactor) (*StakecheckTransactor, error) {
	contract, err := bindStakecheck(address, nil, transactor)
	if err != nil {
		return nil, err
	}
	return &StakecheckTransactor{contract: contract}, nil
}

// bindStakecheck binds a generic wrapper to an already deployed contract.
func bindStakecheck(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor) (*bind.BoundContract, error) {
	parsed, err := abi.JSON(strings.NewReader(StakecheckABI))
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, parsed, caller, transactor), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_Stakecheck *StakecheckRaw) Call(opts *bind.CallOpts, result interface{}, method string, params ...interface{}) error {
	return _Stakecheck.Contract.StakecheckCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_Stakecheck *StakecheckRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _Stakecheck.Contract.StakecheckTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_Stakecheck *StakecheckRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _Stakecheck.Contract.StakecheckTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_Stakecheck *StakecheckCallerRaw) Call(opts *bind.CallOpts, result interface{}, method string, params ...interface{}) error {
	return _Stakecheck.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_Stakecheck *StakecheckTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _Stakecheck.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_Stakecheck *StakecheckTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _Stakecheck.Contract.contract.Transact(opts, method, params...)
}

// Allowance is a free data retrieval call binding the contract method 0xdd62ed3e.
//
// Solidity: function allowance(_owner address, _spender address) constant returns(uint256)
func (_Stakecheck *StakecheckCaller) Allowance(opts *bind.CallOpts, _owner common.Address, _spender common.Address) (*big.Int, error) {
	var (
		ret0 = new(*big.Int)
	)
	out := ret0
	err := _Stakecheck.contract.Call(opts, out, "allowance", _owner, _spender)
	return *ret0, err
}

// Allowance is a free data retrieval call binding the contract method 0xdd62ed3e.
//
// Solidity: function allowance(_owner address, _spender address) constant returns(uint256)
func (_Stakecheck *StakecheckSession) Allowance(_owner common.Address, _spender common.Address) (*big.Int, error) {
	return _Stakecheck.Contract.Allowance(&_Stakecheck.CallOpts, _owner, _spender)
}

// Allowance is a free data retrieval call binding the contract method 0xdd62ed3e.
//
// Solidity: function allowance(_owner address, _spender address) constant returns(uint256)
func (_Stakecheck *StakecheckCallerSession) Allowance(_owner common.Address, _spender common.Address) (*big.Int, error) {
	return _Stakecheck.Contract.Allowance(&_Stakecheck.CallOpts, _owner, _spender)
}

// BalanceOf is a free data retrieval call binding the contract method 0x70a08231.
//
// Solidity: function balanceOf(_owner address) constant returns(uint256)
func (_Stakecheck *StakecheckCaller) BalanceOf(opts *bind.CallOpts, _owner common.Address) (*big.Int, error) {
	var (
		ret0 = new(*big.Int)
	)
	out := ret0
	err := _Stakecheck.contract.Call(opts, out, "balanceOf", _owner)
	return *ret0, err
}

// BalanceOf is a free data retrieval call binding the contract method 0x70a08231.
//
// Solidity: function balanceOf(_owner address) constant returns(uint256)
func (_Stakecheck *StakecheckSession) BalanceOf(_owner common.Address) (*big.Int, error) {
	return _Stakecheck.Contract.BalanceOf(&_Stakecheck.CallOpts, _owner)
}

// BalanceOf is a free data retrieval call binding the contract method 0x70a08231.
//
// Solidity: function balanceOf(_owner address) constant returns(uint256)
func (_Stakecheck *StakecheckCallerSession) BalanceOf(_owner common.Address) (*big.Int, error) {
	return _Stakecheck.Contract.BalanceOf(&_Stakecheck.CallOpts, _owner)
}

// CheckStake is a free data retrieval call binding the contract method 0x90d96d76.
//
// Solidity: function checkStake(_addr address) constant returns(uint256)
func (_Stakecheck *StakecheckCaller) CheckStake(opts *bind.CallOpts, _addr common.Address) (*big.Int, error) {
	var (
		ret0 = new(*big.Int)
	)
	out := ret0
	err := _Stakecheck.contract.Call(opts, out, "checkStake", _addr)
	return *ret0, err
}

// CheckStake is a free data retrieval call binding the contract method 0x90d96d76.
//
// Solidity: function checkStake(_addr address) constant returns(uint256)
func (_Stakecheck *StakecheckSession) CheckStake(_addr common.Address) (*big.Int, error) {
	return _Stakecheck.Contract.CheckStake(&_Stakecheck.CallOpts, _addr)
}

// CheckStake is a free data retrieval call binding the contract method 0x90d96d76.
//
// Solidity: function checkStake(_addr address) constant returns(uint256)
func (_Stakecheck *StakecheckCallerSession) CheckStake(_addr common.Address) (*big.Int, error) {
	return _Stakecheck.Contract.CheckStake(&_Stakecheck.CallOpts, _addr)
}

// Owner is a free data retrieval call binding the contract method 0x8da5cb5b.
//
// Solidity: function owner() constant returns(address)
func (_Stakecheck *StakecheckCaller) Owner(opts *bind.CallOpts) (common.Address, error) {
	var (
		ret0 = new(common.Address)
	)
	out := ret0
	err := _Stakecheck.contract.Call(opts, out, "owner")
	return *ret0, err
}

// Owner is a free data retrieval call binding the contract method 0x8da5cb5b.
//
// Solidity: function owner() constant returns(address)
func (_Stakecheck *StakecheckSession) Owner() (common.Address, error) {
	return _Stakecheck.Contract.Owner(&_Stakecheck.CallOpts)
}

// Owner is a free data retrieval call binding the contract method 0x8da5cb5b.
//
// Solidity: function owner() constant returns(address)
func (_Stakecheck *StakecheckCallerSession) Owner() (common.Address, error) {
	return _Stakecheck.Contract.Owner(&_Stakecheck.CallOpts)
}

// TotalSupply is a free data retrieval call binding the contract method 0x18160ddd.
//
// Solidity: function totalSupply() constant returns(uint256)
func (_Stakecheck *StakecheckCaller) TotalSupply(opts *bind.CallOpts) (*big.Int, error) {
	var (
		ret0 = new(*big.Int)
	)
	out := ret0
	err := _Stakecheck.contract.Call(opts, out, "totalSupply")
	return *ret0, err
}

// TotalSupply is a free data retrieval call binding the contract method 0x18160ddd.
//
// Solidity: function totalSupply() constant returns(uint256)
func (_Stakecheck *StakecheckSession) TotalSupply() (*big.Int, error) {
	return _Stakecheck.Contract.TotalSupply(&_Stakecheck.CallOpts)
}

// TotalSupply is a free data retrieval call binding the contract method 0x18160ddd.
//
// Solidity: function totalSupply() constant returns(uint256)
func (_Stakecheck *StakecheckCallerSession) TotalSupply() (*big.Int, error) {
	return _Stakecheck.Contract.TotalSupply(&_Stakecheck.CallOpts)
}

// AddStake is a paid mutator transaction binding the contract method 0x2d49aa1c.
//
// Solidity: function addStake(_value uint256, _addr address) returns()
func (_Stakecheck *StakecheckTransactor) AddStake(opts *bind.TransactOpts, _value *big.Int, _addr common.Address) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "addStake", _value, _addr)
}

// AddStake is a paid mutator transaction binding the contract method 0x2d49aa1c.
//
// Solidity: function addStake(_value uint256, _addr address) returns()
func (_Stakecheck *StakecheckSession) AddStake(_value *big.Int, _addr common.Address) (*types.Transaction, error) {
	return _Stakecheck.Contract.AddStake(&_Stakecheck.TransactOpts, _value, _addr)
}

// AddStake is a paid mutator transaction binding the contract method 0x2d49aa1c.
//
// Solidity: function addStake(_value uint256, _addr address) returns()
func (_Stakecheck *StakecheckTransactorSession) AddStake(_value *big.Int, _addr common.Address) (*types.Transaction, error) {
	return _Stakecheck.Contract.AddStake(&_Stakecheck.TransactOpts, _value, _addr)
}

// Approve is a paid mutator transaction binding the contract method 0x095ea7b3.
//
// Solidity: function approve(_spender address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckTransactor) Approve(opts *bind.TransactOpts, _spender common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "approve", _spender, _value)
}

// Approve is a paid mutator transaction binding the contract method 0x095ea7b3.
//
// Solidity: function approve(_spender address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckSession) Approve(_spender common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.Approve(&_Stakecheck.TransactOpts, _spender, _value)
}

// Approve is a paid mutator transaction binding the contract method 0x095ea7b3.
//
// Solidity: function approve(_spender address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckTransactorSession) Approve(_spender common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.Approve(&_Stakecheck.TransactOpts, _spender, _value)
}

// DecreaseApproval is a paid mutator transaction binding the contract method 0x66188463.
//
// Solidity: function decreaseApproval(_spender address, _subtractedValue uint256) returns(bool)
func (_Stakecheck *StakecheckTransactor) DecreaseApproval(opts *bind.TransactOpts, _spender common.Address, _subtractedValue *big.Int) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "decreaseApproval", _spender, _subtractedValue)
}

// DecreaseApproval is a paid mutator transaction binding the contract method 0x66188463.
//
// Solidity: function decreaseApproval(_spender address, _subtractedValue uint256) returns(bool)
func (_Stakecheck *StakecheckSession) DecreaseApproval(_spender common.Address, _subtractedValue *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.DecreaseApproval(&_Stakecheck.TransactOpts, _spender, _subtractedValue)
}

// DecreaseApproval is a paid mutator transaction binding the contract method 0x66188463.
//
// Solidity: function decreaseApproval(_spender address, _subtractedValue uint256) returns(bool)
func (_Stakecheck *StakecheckTransactorSession) DecreaseApproval(_spender common.Address, _subtractedValue *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.DecreaseApproval(&_Stakecheck.TransactOpts, _spender, _subtractedValue)
}

// IncreaseApproval is a paid mutator transaction binding the contract method 0xd73dd623.
//
// Solidity: function increaseApproval(_spender address, _addedValue uint256) returns(bool)
func (_Stakecheck *StakecheckTransactor) IncreaseApproval(opts *bind.TransactOpts, _spender common.Address, _addedValue *big.Int) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "increaseApproval", _spender, _addedValue)
}

// IncreaseApproval is a paid mutator transaction binding the contract method 0xd73dd623.
//
// Solidity: function increaseApproval(_spender address, _addedValue uint256) returns(bool)
func (_Stakecheck *StakecheckSession) IncreaseApproval(_spender common.Address, _addedValue *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.IncreaseApproval(&_Stakecheck.TransactOpts, _spender, _addedValue)
}

// IncreaseApproval is a paid mutator transaction binding the contract method 0xd73dd623.
//
// Solidity: function increaseApproval(_spender address, _addedValue uint256) returns(bool)
func (_Stakecheck *StakecheckTransactorSession) IncreaseApproval(_spender common.Address, _addedValue *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.IncreaseApproval(&_Stakecheck.TransactOpts, _spender, _addedValue)
}

// ReleaseStake is a paid mutator transaction binding the contract method 0x3b249039.
//
// Solidity: function releaseStake(_value uint256, _addr address) returns()
func (_Stakecheck *StakecheckTransactor) ReleaseStake(opts *bind.TransactOpts, _value *big.Int, _addr common.Address) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "releaseStake", _value, _addr)
}

// ReleaseStake is a paid mutator transaction binding the contract method 0x3b249039.
//
// Solidity: function releaseStake(_value uint256, _addr address) returns()
func (_Stakecheck *StakecheckSession) ReleaseStake(_value *big.Int, _addr common.Address) (*types.Transaction, error) {
	return _Stakecheck.Contract.ReleaseStake(&_Stakecheck.TransactOpts, _value, _addr)
}

// ReleaseStake is a paid mutator transaction binding the contract method 0x3b249039.
//
// Solidity: function releaseStake(_value uint256, _addr address) returns()
func (_Stakecheck *StakecheckTransactorSession) ReleaseStake(_value *big.Int, _addr common.Address) (*types.Transaction, error) {
	return _Stakecheck.Contract.ReleaseStake(&_Stakecheck.TransactOpts, _value, _addr)
}

// Transfer is a paid mutator transaction binding the contract method 0xa9059cbb.
//
// Solidity: function transfer(_to address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckTransactor) Transfer(opts *bind.TransactOpts, _to common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "transfer", _to, _value)
}

// Transfer is a paid mutator transaction binding the contract method 0xa9059cbb.
//
// Solidity: function transfer(_to address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckSession) Transfer(_to common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.Transfer(&_Stakecheck.TransactOpts, _to, _value)
}

// Transfer is a paid mutator transaction binding the contract method 0xa9059cbb.
//
// Solidity: function transfer(_to address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckTransactorSession) Transfer(_to common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.Transfer(&_Stakecheck.TransactOpts, _to, _value)
}

// TransferFrom is a paid mutator transaction binding the contract method 0x23b872dd.
//
// Solidity: function transferFrom(_from address, _to address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckTransactor) TransferFrom(opts *bind.TransactOpts, _from common.Address, _to common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "transferFrom", _from, _to, _value)
}

// TransferFrom is a paid mutator transaction binding the contract method 0x23b872dd.
//
// Solidity: function transferFrom(_from address, _to address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckSession) TransferFrom(_from common.Address, _to common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.TransferFrom(&_Stakecheck.TransactOpts, _from, _to, _value)
}

// TransferFrom is a paid mutator transaction binding the contract method 0x23b872dd.
//
// Solidity: function transferFrom(_from address, _to address, _value uint256) returns(bool)
func (_Stakecheck *StakecheckTransactorSession) TransferFrom(_from common.Address, _to common.Address, _value *big.Int) (*types.Transaction, error) {
	return _Stakecheck.Contract.TransferFrom(&_Stakecheck.TransactOpts, _from, _to, _value)
}

// TransferOwnership is a paid mutator transaction binding the contract method 0xf2fde38b.
//
// Solidity: function transferOwnership(newOwner address) returns()
func (_Stakecheck *StakecheckTransactor) TransferOwnership(opts *bind.TransactOpts, newOwner common.Address) (*types.Transaction, error) {
	return _Stakecheck.contract.Transact(opts, "transferOwnership", newOwner)
}

// TransferOwnership is a paid mutator transaction binding the contract method 0xf2fde38b.
//
// Solidity: function transferOwnership(newOwner address) returns()
func (_Stakecheck *StakecheckSession) TransferOwnership(newOwner common.Address) (*types.Transaction, error) {
	return _Stakecheck.Contract.TransferOwnership(&_Stakecheck.TransactOpts, newOwner)
}

// TransferOwnership is a paid mutator transaction binding the contract method 0xf2fde38b.
//
// Solidity: function transferOwnership(newOwner address) returns()
func (_Stakecheck *StakecheckTransactorSession) TransferOwnership(newOwner common.Address) (*types.Transaction, error) {
	return _Stakecheck.Contract.TransferOwnership(&_Stakecheck.TransactOpts, newOwner)
}

func instantiateStakecheck() {
	// Create an IPC based RPC connection to a remote node
	conn, err := ethclient.Dial("/home/nishant/XDC01-docker-Nnodes/static-nodes/qdata_1/dd/")
	if err != nil {
		log.Info("Failed to connect to the Ethereum client: %v", err)
	}
	// Instantiate the contract and display its name
	stakecheck, err := NewStakecheck(common.HexToAddress("0xC4c83D2917FF28DbfDF5E1b8173AFac9AEb90fc5"), conn)
	if err != nil {
		log.Info("Failed to instantiate Stakecheck contract: %v", err)
	}
	if stakecheck!=nil{
		log.Info("Instantiated")
	}
	addr := common.HexToAddress("0x0638e1574728b6d862dd5d3a3e0942c3be47d996")

	//// Create an authorized transactor
	//auth, err := bind.NewTransactor(strings.NewReader(key), "")
	//if err!= nil {
	//	log.Info("Failed to create authorized transactor: %v", err)
	//}

	stakes, err := stakecheck.CheckStake(&bind.CallOpts{Pending: true},addr)
	if err != nil {
		log.Info("Failed to receive stakes: %v", err)
	}
	if stakes != nil{
		log.Info("Stake check initiated")
	}
}

