// Copyright 2016 The go-ethereum Authors
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

package ethclient_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/internal/ethapi/override"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// Verify that Client implements the ethereum interfaces.
var (
	_ = ethereum.ChainReader(&ethclient.Client{})
	_ = ethereum.TransactionReader(&ethclient.Client{})
	_ = ethereum.ChainStateReader(&ethclient.Client{})
	_ = ethereum.ChainSyncReader(&ethclient.Client{})
	_ = ethereum.ContractCaller(&ethclient.Client{})
	_ = ethereum.GasEstimator(&ethclient.Client{})
	_ = ethereum.GasPricer(&ethclient.Client{})
	_ = ethereum.LogFilterer(&ethclient.Client{})
	_ = ethereum.PendingStateReader(&ethclient.Client{})
	// _ = ethereum.PendingStateEventer(&ethclient.Client{})
	_ = ethereum.PendingContractCaller(&ethclient.Client{})
)

var (
	testKey, _         = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	testAddr           = crypto.PubkeyToAddress(testKey.PublicKey)
	testBalance        = big.NewInt(2e15)
	revertContractAddr = common.HexToAddress("290f1b36649a61e369c6276f6d29463335b4400c")
	revertCode         = common.FromHex("7f08c379a0000000000000000000000000000000000000000000000000000000006000526020600452600a6024527f75736572206572726f7200000000000000000000000000000000000000000000604452604e6000fd")
)

var genesis = &core.Genesis{
	Config: params.AllDevChainProtocolChanges,
	Alloc: types.GenesisAlloc{
		testAddr:           {Balance: testBalance},
		revertContractAddr: {Code: revertCode},
	},
	ExtraData: []byte("test genesis"),
	Timestamp: 9000,
	BaseFee:   big.NewInt(params.InitialBaseFee),
}

var genesisForHistorical = &core.Genesis{
	Config:    params.OptimismTestCliqueConfig,
	Alloc:     types.GenesisAlloc{testAddr: {Balance: testBalance}},
	ExtraData: []byte("test genesis"),
	Timestamp: 9000,
	BaseFee:   big.NewInt(params.InitialBaseFee),
}

var depositTx = types.NewTx(&types.DepositTx{
	Value: big.NewInt(12),
	Gas:   params.TxGas + 2000,
	To:    &common.Address{2},
	Data:  make([]byte, 500),
})

var testTx1 = types.MustSignNewTx(testKey, types.LatestSigner(genesis.Config), &types.LegacyTx{
	Nonce:    0,
	Value:    big.NewInt(12),
	GasPrice: big.NewInt(params.InitialBaseFee),
	Gas:      params.TxGas,
	To:       &common.Address{2},
})

var testTx2 = types.MustSignNewTx(testKey, types.LatestSigner(genesis.Config), &types.LegacyTx{
	Nonce:    1,
	Value:    big.NewInt(8),
	GasPrice: big.NewInt(params.InitialBaseFee),
	Gas:      params.TxGas,
	To:       &common.Address{2},
})

type mockHistoricalBackend struct{}

func (m *mockHistoricalBackend) Call(ctx context.Context, args ethapi.TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash, overrides *override.StateOverride) (hexutil.Bytes, error) {
	num, ok := blockNrOrHash.Number()
	if ok && num == 1 {
		return hexutil.Bytes("test"), nil
	}
	return nil, ethereum.NotFound
}

func (m *mockHistoricalBackend) EstimateGas(ctx context.Context, args ethapi.TransactionArgs, blockNrOrHash *rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	num, ok := blockNrOrHash.Number()
	if ok && num == 1 {
		return hexutil.Uint64(12345), nil
	}
	return 0, ethereum.NotFound
}

func newMockHistoricalBackend(t *testing.T) string {
	s := rpc.NewServer()
	err := node.RegisterApis([]rpc.API{
		{
			Namespace:     "eth",
			Service:       new(mockHistoricalBackend),
			Public:        true,
			Authenticated: false,
		},
	}, nil, s)
	if err != nil {
		t.Fatalf("error creating mock historical backend: %v", err)
	}

	hdlr := node.NewHTTPHandlerStack(s, []string{"*"}, []string{"*"}, nil)
	mux := http.NewServeMux()
	mux.Handle("/", hdlr)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("error creating mock historical backend listener: %v", err)
	}

	go func() {
		httpS := &http.Server{Handler: mux}
		httpS.Serve(listener)

		t.Cleanup(func() {
			httpS.Shutdown(context.Background())
		})
	}()

	return fmt.Sprintf("http://%s", listener.Addr().String())
}

func newTestBackend(t *testing.T, config *node.Config, enableHistoricalState bool) (*node.Node, []*types.Block, error) {
	var actualGenesis *core.Genesis
	var chainLength int
	if enableHistoricalState {
		actualGenesis = genesisForHistorical
		chainLength = 10
	} else {
		actualGenesis = genesis
		chainLength = 2
	}

	// Generate test chain.
	blocks := generateTestChain(actualGenesis, chainLength)

	// Create node
	if config == nil {
		config = new(node.Config)
	}
	n, err := node.New(config)
	if err != nil {
		return nil, nil, fmt.Errorf("can't create new node: %v", err)
	}
	// Create Ethereum Service
	ecfg := &ethconfig.Config{Genesis: actualGenesis, RPCGasCap: 1000000}
	if enableHistoricalState {
		histAddr := newMockHistoricalBackend(t)
		ecfg.RollupHistoricalRPC = histAddr
		ecfg.RollupHistoricalRPCTimeout = time.Second * 5
	}
	ethservice, err := eth.New(n, ecfg)
	if err != nil {
		return nil, nil, fmt.Errorf("can't create new ethereum service: %v", err)
	}

	// Ensure tx pool starts the background operation
	txPool := ethservice.TxPool()
	if err = txPool.Sync(); err != nil {
		return nil, nil, fmt.Errorf("can't sync transaction pool: %v", err)
	}

	if enableHistoricalState { // swap to the pre-bedrock consensus-engine that we used to generate the historical blocks
		ethservice.BlockChain().Engine().(*beacon.Beacon).SwapInner(ethash.NewFaker())
	}

	// Import the test chain.
	if err := n.Start(); err != nil {
		return nil, nil, fmt.Errorf("can't start test node: %v", err)
	}
	if _, err := ethservice.BlockChain().InsertChain(blocks[1:]); err != nil {
		return nil, nil, fmt.Errorf("can't import test blocks: %v", err)
	}

	// Ensure the tx indexing is fully generated
	for ; ; time.Sleep(time.Millisecond * 100) {
		progress, err := ethservice.BlockChain().TxIndexProgress()
		if err == nil && progress.Done() {
			break
		}
	}

	if enableHistoricalState {
		// Now that we have a filled DB, swap the pre-Bedrock consensus to OpLegacy,
		// which does not support re-processing of pre-bedrock data.
		ethservice.Engine().(*beacon.Beacon).SwapInner(&beacon.OpLegacy{})
	}

	return n, blocks, nil
}

func generateTestChain(genesis *core.Genesis, length int) []*types.Block {
	generate := func(i int, g *core.BlockGen) {
		g.OffsetTime(5)
		g.SetExtra([]byte("test"))
		if i == 1 {
			// Test transactions are included in block #2.
			if genesis.Config.Optimism != nil && genesis.Config.IsBedrock(big.NewInt(1)) {
				g.AddTx(depositTx)
			}
			g.AddTx(testTx1)
			g.AddTx(testTx2)
		}
	}
	_, blocks, _ := core.GenerateChainWithGenesis(genesis, beacon.New(ethash.NewFaker()), length, generate)
	return append([]*types.Block{genesis.ToBlock()}, blocks...)
}

func TestEthClientHistoricalBackend(t *testing.T) {
	backend, _, err := newTestBackend(t, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	client := backend.Attach()
	defer backend.Close()
	defer client.Close()

	testHistoricalRPC(t, client)
}

func TestEthClient(t *testing.T) {
	backend, chain, err := newTestBackend(t, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	client := backend.Attach()
	defer backend.Close()
	defer client.Close()

	tests := map[string]struct {
		test func(t *testing.T)
	}{
		"Header": {
			func(t *testing.T) { testHeader(t, chain, client) },
		},
		"BalanceAt": {
			func(t *testing.T) { testBalanceAt(t, client) },
		},
		"TxInBlockInterrupted": {
			func(t *testing.T) { testTransactionInBlock(t, client) },
		},
		"ChainID": {
			func(t *testing.T) { testChainID(t, client) },
		},
		"GetBlock": {
			func(t *testing.T) { testGetBlock(t, client) },
		},
		"StatusFunctions": {
			func(t *testing.T) { testStatusFunctions(t, client) },
		},
		"CallContract": {
			func(t *testing.T) { testCallContract(t, client) },
		},
		"CallContractAtHash": {
			func(t *testing.T) { testCallContractAtHash(t, client) },
		},
		"AtFunctions": {
			func(t *testing.T) { testAtFunctions(t, client) },
		},
		"TransactionSender": {
			func(t *testing.T) { testTransactionSender(t, client) },
		},
		"EstimateGas": {
			func(t *testing.T) { testEstimateGas(t, client) },
		},
	}

	t.Parallel()
	for name, tt := range tests {
		t.Run(name, tt.test)
	}
}

func testHeader(t *testing.T, chain []*types.Block, client *rpc.Client) {
	tests := map[string]struct {
		block   *big.Int
		want    *types.Header
		wantErr error
	}{
		"genesis": {
			block: big.NewInt(0),
			want:  chain[0].Header(),
		},
		"first_block": {
			block: big.NewInt(1),
			want:  chain[1].Header(),
		},
		"future_block": {
			block:   big.NewInt(1000000000),
			want:    nil,
			wantErr: ethereum.NotFound,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ec := ethclient.NewClient(client)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			got, err := ec.HeaderByNumber(ctx, tt.block)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("HeaderByNumber(%v) error = %q, want %q", tt.block, err, tt.wantErr)
			}
			if got != nil && got.Number != nil && got.Number.Sign() == 0 {
				got.Number = big.NewInt(0) // hack to make DeepEqual work
			}
			if got.Hash() != tt.want.Hash() {
				t.Fatalf("HeaderByNumber(%v) got = %v, want %v", tt.block, got, tt.want)
			}
		})
	}
}

func testBalanceAt(t *testing.T, client *rpc.Client) {
	tests := map[string]struct {
		account common.Address
		block   *big.Int
		want    *big.Int
		wantErr error
	}{
		"valid_account_genesis": {
			account: testAddr,
			block:   big.NewInt(0),
			want:    testBalance,
		},
		"valid_account": {
			account: testAddr,
			block:   big.NewInt(1),
			want:    testBalance,
		},
		"non_existent_account": {
			account: common.Address{1},
			block:   big.NewInt(1),
			want:    big.NewInt(0),
		},
		"future_block": {
			account: testAddr,
			block:   big.NewInt(1000000000),
			want:    big.NewInt(0),
			wantErr: errors.New("header not found"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ec := ethclient.NewClient(client)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			got, err := ec.BalanceAt(ctx, tt.account, tt.block)
			if tt.wantErr != nil && (err == nil || err.Error() != tt.wantErr.Error()) {
				t.Fatalf("BalanceAt(%x, %v) error = %q, want %q", tt.account, tt.block, err, tt.wantErr)
			}
			if got.Cmp(tt.want) != 0 {
				t.Fatalf("BalanceAt(%x, %v) = %v, want %v", tt.account, tt.block, got, tt.want)
			}
		})
	}
}

func TestSonic(t *testing.T) {
	// ec := ethclient.NewClient(client)
	data := `{"baseFeePerGas":"0x2da282a8","blobGasUsed":"0x0","difficulty":"0x0","excessBlobGas":"0x0","extraData":"0x74657374","gasLimit":"0x47e7c4","gasUsed":"0xa410","hash":"0x84c82d89ac0fd95a30e4ee94696102051f5a26daa4a1001118c09610f3294c4c","logsBloom":"0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000","miner":"0x0000000000000000000000000000000000000000","mixHash":"0x0000000000000000000000000000000000000000000000000000000000000000","nonce":"0x0000000000000000","number":"0x2","parentBeaconBlockRoot":"0x0000000000000000000000000000000000000000000000000000000000000000","parentHash":"0x3405339aee61e7a887d432c24a4c6dda9a675a4c9e867ba0db4455d2dc87e73b","receiptsRoot":"0xd95b673818fa493deec414e01e610d97ee287c9421c8eff4102b1647c1a184e4","requestsHash":"0xe3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","sha3Uncles":"0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347","size":"0x33a","stateRoot":"0x7a0ac406393f238877ee8030400dfffa7cd78fc8209dbdc6379e7f90f97a7763","timestamp":"0x2346","transactions":[{"blockHash":"0x84c82d89ac0fd95a30e4ee94696102051f5a26daa4a1001118c09610f3294c4c","blockNumber":"0x2","from":"0x71562b71999873db5b286df957af199ec94617f7","gas":"0x5208","gasPrice":"0x3b9aca00","hash":"0xcf02273f2b656c3d919e68d3fbf3adc44f7a376cc0b97350d6f333ec0f24236e","input":"0x","nonce":"0x0","to":"0x0200000000000000000000000000000000000000","transactionIndex":"0x0","value":"0xc","type":"0x0","chainId":"0x539","v":"0xa96","r":"0xa2fe848dd9e03cc0bd52ad1453b037e580daade904bee80642bed2a0f377b00b","s":"0x6e9d208eab4bdc059a21236e088e8140bda1c717da4ed9c163546b22ce12868c"},{"blockHash":"0x84c82d89ac0fd95a30e4ee94696102051f5a26daa4a1001118c09610f3294c4c","blockNumber":"0x2","from":"0x71562b71999873db5b286df957af199ec94617f7","gas":"0x5208","gasPrice":"0x3b9aca00","hash":"0xda46ac493dab369bb67792e21229ee00ae32326fca8160abd6960b700c043925","input":"0x","nonce":"0x1","to":"0x0200000000000000000000000000000000000000","transactionIndex":"0x1","value":"0x8","type":"0x0","chainId":"0x539","v":"0xa96","r":"0x18aaad05374d5d844203e0fe7e3bd0d7b66787a8069da7abb2ef993bd58ff9a9","s":"0xcc8a1cc7ff1000e7f9c1085ca2738dfdf15dfc0c7273c6d9fc913bd277bb65f"}],"transactionsRoot":"0x542eb824314ce48fc367ef410035b895450119f71ef82b80e5ccbfcfd72c42d8","uncles":[],"withdrawals":[],"withdrawalsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"}`
	res := ethclient.FullRpcBlock{
		Header: &types.Header{},
	}
	sonic.ConfigFastest.Unmarshal([]byte(data), &res)
	fmt.Println(res)
	fmt.Println(res.Header)
	fmt.Println("hash", res.RpcBlock.Hash)
	fmt.Println("transactions", len(res.RpcBlock.Transactions))
}

func testTransactionInBlock(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	// Get current block by number.
	block, err := ec.BlockByNumber(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Test tx in block not found.
	if _, err := ec.TransactionInBlock(context.Background(), block.Hash(), 20); err != ethereum.NotFound {
		t.Fatal("error should be ethereum.NotFound")
	}

	// Test tx in block found.
	tx, err := ec.TransactionInBlock(context.Background(), block.Hash(), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx.Hash() != testTx1.Hash() {
		t.Fatalf("unexpected transaction: %v", tx)
	}

	tx, err = ec.TransactionInBlock(context.Background(), block.Hash(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx.Hash() != testTx2.Hash() {
		t.Fatalf("unexpected transaction: %v", tx)
	}

	// Test pending block
	_, err = ec.BlockByNumber(context.Background(), big.NewInt(int64(rpc.PendingBlockNumber)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func testChainID(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)
	id, err := ec.ChainID(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == nil || id.Cmp(params.AllDevChainProtocolChanges.ChainID) != 0 {
		t.Fatalf("ChainID returned wrong number: %+v", id)
	}
}

func testGetBlock(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	// Get current block number
	blockNumber, err := ec.BlockNumber(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blockNumber != 2 {
		t.Fatalf("BlockNumber returned wrong number: %d", blockNumber)
	}
	// Get current block by number
	block, err := ec.BlockByNumber(context.Background(), new(big.Int).SetUint64(blockNumber))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block.NumberU64() != blockNumber {
		t.Fatalf("BlockByNumber returned wrong block: want %d got %d", blockNumber, block.NumberU64())
	}
	// Get current block by hash
	blockH, err := ec.BlockByHash(context.Background(), block.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block.Hash() != blockH.Hash() {
		t.Fatalf("BlockByHash returned wrong block: want %v got %v", block.Hash().Hex(), blockH.Hash().Hex())
	}
	// Get header by number
	header, err := ec.HeaderByNumber(context.Background(), new(big.Int).SetUint64(blockNumber))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block.Header().Hash() != header.Hash() {
		t.Fatalf("HeaderByNumber returned wrong header: want %v got %v", block.Header().Hash().Hex(), header.Hash().Hex())
	}
	// Get header by hash
	headerH, err := ec.HeaderByHash(context.Background(), block.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block.Header().Hash() != headerH.Hash() {
		t.Fatalf("HeaderByHash returned wrong header: want %v got %v", block.Header().Hash().Hex(), headerH.Hash().Hex())
	}
}

func testStatusFunctions(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	// Sync progress
	progress, err := ec.SyncProgress(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if progress != nil {
		t.Fatalf("unexpected progress: %v", progress)
	}

	// NetworkID
	networkID, err := ec.NetworkID(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if networkID.Cmp(big.NewInt(1337)) != 0 {
		t.Fatalf("unexpected networkID: %v", networkID)
	}

	// SuggestGasPrice
	gasPrice, err := ec.SuggestGasPrice(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gasPrice.Cmp(big.NewInt(1000000000)) != 0 {
		t.Fatalf("unexpected gas price: %v", gasPrice)
	}

	// SuggestGasTipCap
	gasTipCap, err := ec.SuggestGasTipCap(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gasTipCap.Cmp(big.NewInt(234375000)) != 0 {
		t.Fatalf("unexpected gas tip cap: %v", gasTipCap)
	}

	// BlobBaseFee
	blobBaseFee, err := ec.BlobBaseFee(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blobBaseFee.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("unexpected blob base fee: %v", blobBaseFee)
	}

	// FeeHistory
	history, err := ec.FeeHistory(context.Background(), 1, big.NewInt(2), []float64{95, 99})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := &ethereum.FeeHistory{
		OldestBlock: big.NewInt(2),
		Reward: [][]*big.Int{
			{
				big.NewInt(234375000),
				big.NewInt(234375000),
			},
		},
		BaseFee: []*big.Int{
			big.NewInt(765625000),
			big.NewInt(671627818),
		},
		GasUsedRatio: []float64{0.008912678667376286},
	}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("FeeHistory result doesn't match expected: (got: %v, want: %v)", history, want)
	}
}

func testCallContractAtHash(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	// EstimateGas
	msg := ethereum.CallMsg{
		From:  testAddr,
		To:    &common.Address{},
		Gas:   21000,
		Value: big.NewInt(1),
	}
	gas, err := ec.EstimateGas(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gas != 21000 {
		t.Fatalf("unexpected gas price: %v", gas)
	}
	block, err := ec.HeaderByNumber(context.Background(), big.NewInt(1))
	if err != nil {
		t.Fatalf("BlockByNumber error: %v", err)
	}
	// CallContract
	if _, err := ec.CallContractAtHash(context.Background(), msg, block.Hash()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func testCallContract(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	// EstimateGas
	msg := ethereum.CallMsg{
		From:  testAddr,
		To:    &common.Address{},
		Gas:   21000,
		Value: big.NewInt(1),
	}
	gas, err := ec.EstimateGas(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gas != 21000 {
		t.Fatalf("unexpected gas price: %v", gas)
	}
	// CallContract
	if _, err := ec.CallContract(context.Background(), msg, big.NewInt(1)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// PendingCallContract
	if _, err := ec.PendingCallContract(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func testAtFunctions(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	block, err := ec.HeaderByNumber(context.Background(), big.NewInt(1))
	if err != nil {
		t.Fatalf("BlockByNumber error: %v", err)
	}

	// send a transaction for some interesting pending status
	if err := sendTransaction(ec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// wait for the transaction to be included in the pending block
	for {
		// Check pending transaction count
		pending, err := ec.PendingTransactionCount(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pending == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Query balance
	balance, err := ec.BalanceAt(context.Background(), testAddr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hashBalance, err := ec.BalanceAtHash(context.Background(), testAddr, block.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if balance.Cmp(hashBalance) == 0 {
		t.Fatalf("unexpected balance at hash: %v %v", balance, hashBalance)
	}
	penBalance, err := ec.PendingBalanceAt(context.Background(), testAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if balance.Cmp(penBalance) == 0 {
		t.Fatalf("unexpected balance: %v %v", balance, penBalance)
	}
	// NonceAt
	nonce, err := ec.NonceAt(context.Background(), testAddr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hashNonce, err := ec.NonceAtHash(context.Background(), testAddr, block.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hashNonce == nonce {
		t.Fatalf("unexpected nonce at hash: %v %v", nonce, hashNonce)
	}
	penNonce, err := ec.PendingNonceAt(context.Background(), testAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if penNonce != nonce+1 {
		t.Fatalf("unexpected nonce: %v %v", nonce, penNonce)
	}
	// StorageAt
	storage, err := ec.StorageAt(context.Background(), testAddr, common.Hash{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hashStorage, err := ec.StorageAtHash(context.Background(), testAddr, common.Hash{}, block.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(storage, hashStorage) {
		t.Fatalf("unexpected storage at hash: %v %v", storage, hashStorage)
	}
	penStorage, err := ec.PendingStorageAt(context.Background(), testAddr, common.Hash{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(storage, penStorage) {
		t.Fatalf("unexpected storage: %v %v", storage, penStorage)
	}
	// CodeAt
	code, err := ec.CodeAt(context.Background(), testAddr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hashCode, err := ec.CodeAtHash(context.Background(), common.Address{}, block.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(code, hashCode) {
		t.Fatalf("unexpected code at hash: %v %v", code, hashCode)
	}
	penCode, err := ec.PendingCodeAt(context.Background(), testAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(code, penCode) {
		t.Fatalf("unexpected code: %v %v", code, penCode)
	}
	// Use HeaderByNumber to get a header for EstimateGasAtBlock and EstimateGasAtBlockHash
	latestHeader, err := ec.HeaderByNumber(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// EstimateGasAtBlock
	msg := ethereum.CallMsg{
		From:  testAddr,
		To:    &common.Address{},
		Gas:   21000,
		Value: big.NewInt(1),
	}
	gas, err := ec.EstimateGasAtBlock(context.Background(), msg, latestHeader.Number)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gas != 21000 {
		t.Fatalf("unexpected gas limit: %v", gas)
	}
	// EstimateGasAtBlockHash
	gas, err = ec.EstimateGasAtBlockHash(context.Background(), msg, latestHeader.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gas != 21000 {
		t.Fatalf("unexpected gas limit: %v", gas)
	}

	// Verify that sender address of pending transaction is saved in cache.
	pendingBlock, err := ec.BlockByNumber(context.Background(), big.NewInt(int64(rpc.PendingBlockNumber)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No additional RPC should be required, ensure the server is not asked by
	// canceling the context.
	sender, err := ec.TransactionSender(newCanceledContext(), pendingBlock.Transactions()[0], pendingBlock.Hash(), 0)
	if err != nil {
		t.Fatal("unable to recover sender:", err)
	}
	if sender != testAddr {
		t.Fatal("wrong sender:", sender)
	}
}

func testTransactionSender(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)
	ctx := context.Background()

	// Retrieve testTx1 via RPC.
	block2, err := ec.HeaderByNumber(ctx, big.NewInt(2))
	if err != nil {
		t.Fatal("can't get block 1:", err)
	}
	tx1, err := ec.TransactionInBlock(ctx, block2.Hash(), 0)
	if err != nil {
		t.Fatal("can't get tx:", err)
	}
	if tx1.Hash() != testTx1.Hash() {
		t.Fatalf("wrong tx hash %v, want %v", tx1.Hash(), testTx1.Hash())
	}

	// The sender address is cached in tx1, so no additional RPC should be required in
	// TransactionSender. Ensure the server is not asked by canceling the context here.
	sender1, err := ec.TransactionSender(newCanceledContext(), tx1, block2.Hash(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if sender1 != testAddr {
		t.Fatal("wrong sender:", sender1)
	}

	// Now try to get the sender of testTx2, which was not fetched through RPC.
	// TransactionSender should query the server here.
	sender2, err := ec.TransactionSender(ctx, testTx2, block2.Hash(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if sender2 != testAddr {
		t.Fatal("wrong sender:", sender2)
	}
}

func testEstimateGas(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	// EstimateGas
	msg := ethereum.CallMsg{
		From:  testAddr,
		To:    &common.Address{},
		Gas:   21000,
		Value: big.NewInt(1),
	}
	gas, err := ec.EstimateGas(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gas != 21000 {
		t.Fatalf("unexpected gas price: %v", gas)
	}
}

func testHistoricalRPC(t *testing.T, client *rpc.Client) {
	ec := ethclient.NewClient(client)

	// Estimate Gas RPC
	msg := ethereum.CallMsg{
		From:  testAddr,
		To:    &common.Address{},
		Gas:   21000,
		Value: big.NewInt(1),
	}
	var res hexutil.Uint64
	callMsg := map[string]interface{}{
		"from":  msg.From,
		"to":    msg.To,
		"gas":   hexutil.Uint64(msg.Gas),
		"value": (*hexutil.Big)(msg.Value),
	}
	err := client.CallContext(context.Background(), &res, "eth_estimateGas", callMsg, rpc.BlockNumberOrHashWithNumber(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != 12345 {
		t.Fatalf("invalid result: %d", res)
	}

	// Call Contract RPC
	histVal, err := ec.CallContract(context.Background(), msg, big.NewInt(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(histVal) != "test" {
		t.Fatalf("expected %s to equal test", string(histVal))
	}
}

func newCanceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	<-ctx.Done() // Ensure the close of the Done channel
	return ctx
}

func sendTransaction(ec *ethclient.Client) error {
	chainID, err := ec.ChainID(context.Background())
	if err != nil {
		return err
	}
	nonce, err := ec.NonceAt(context.Background(), testAddr, nil)
	if err != nil {
		return err
	}

	signer := types.LatestSignerForChainID(chainID)
	tx, err := types.SignNewTx(testKey, signer, &types.LegacyTx{
		Nonce:    nonce,
		To:       &common.Address{2},
		Value:    big.NewInt(1),
		Gas:      22000,
		GasPrice: big.NewInt(params.InitialBaseFee),
	})
	if err != nil {
		return err
	}
	return ec.SendTransaction(context.Background(), tx)
}

// Here we show how to get the error message of reverted contract call.
func ExampleRevertErrorData() {
	// First create an ethclient.Client instance.
	ctx := context.Background()
	ec, _ := ethclient.DialContext(ctx, exampleNode.HTTPEndpoint())

	// Call the contract.
	// Note we expect the call to return an error.
	contract := common.HexToAddress("290f1b36649a61e369c6276f6d29463335b4400c")
	call := ethereum.CallMsg{To: &contract, Gas: 30000}
	result, err := ec.CallContract(ctx, call, nil)
	if len(result) > 0 {
		panic("got result")
	}
	if err == nil {
		panic("call did not return error")
	}

	// Extract the low-level revert data from the error.
	revertData, ok := ethclient.RevertErrorData(err)
	if !ok {
		panic("unpacking revert failed")
	}
	fmt.Printf("revert: %x\n", revertData)

	// Parse the revert data to obtain the error message.
	message, err := abi.UnpackRevert(revertData)
	if err != nil {
		panic("parsing ABI error failed: " + err.Error())
	}
	fmt.Println("message:", message)

	// Output:
	// revert: 08c379a00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000a75736572206572726f72
	// message: user error
}
