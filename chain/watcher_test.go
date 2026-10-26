package chain

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	testToken = common.HexToAddress("0xdAC17F958D2ee523a2206206994597C13D831ec7")
	testFrom  = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	testTo    = common.HexToAddress("0x00000000000000000000000000000000000000bb")
)

// transferLog 构造一条 Transfer 日志：from / to 落在 topics，金额落在 data。
func transferLog(block uint64, logIndex uint, value *big.Int) types.Log {
	return types.Log{
		Address: testToken,
		Topics: []common.Hash{
			TransferTopic,
			common.BytesToHash(common.LeftPadBytes(testFrom.Bytes(), 32)),
			common.BytesToHash(common.LeftPadBytes(testTo.Bytes(), 32)),
		},
		Data:        common.LeftPadBytes(value.Bytes(), 32),
		BlockNumber: block,
		TxHash:      common.HexToHash(fmt.Sprintf("0x%064x", block)),
		Index:       logIndex,
	}
}

func TestDecodeTransfer(t *testing.T) {
	value, _ := new(big.Int).SetString("1000000000000000000", 10)

	got, ok := DecodeTransfer(transferLog(42, 3, value))
	if !ok {
		t.Fatal("DecodeTransfer returned false for a valid Transfer log")
	}

	if got.BlockNumber != 42 || got.LogIndex != 3 {
		t.Fatalf("block/logIndex = %d/%d, want 42/3", got.BlockNumber, got.LogIndex)
	}
	if got.From != "0x00000000000000000000000000000000000000aa" {
		t.Fatalf("from = %q", got.From)
	}
	if got.To != "0x00000000000000000000000000000000000000bb" {
		t.Fatalf("to = %q", got.To)
	}
	// 18 位小数的金额必须原样保住，float64 在这里会变成 1e18 近似值。
	if got.Value != "1000000000000000000" {
		t.Fatalf("value = %q, want 1000000000000000000", got.Value)
	}
	if got.Token != "0xdac17f958d2ee523a2206206994597c13d831ec7" {
		t.Fatalf("token = %q, want lowercase address", got.Token)
	}
}

func TestDecodeTransferRejectsOtherEvents(t *testing.T) {
	// 同一合约上的 Approval 事件签名不同，必须被跳过而不是当成错误。
	approval := types.Log{
		Address: testToken,
		Topics:  []common.Hash{common.HexToHash("0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925")},
		Data:    make([]byte, 32),
	}
	if _, ok := DecodeTransfer(approval); ok {
		t.Fatal("DecodeTransfer accepted a non-Transfer log")
	}
}

func TestWatcherWaitsForConfirmations(t *testing.T) {
	src := newFakeSource(100)
	sink := &fakeSink{}

	w := NewWatcher(Config{Source: src, Sink: sink, Confirmations: 12})
	if err := w.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	// 没有游标、也没指定起点时只跟新块：100-12 = 88。
	if got := src.queryRanges(); len(got) != 1 || got[0] != [2]uint64{88, 88} {
		t.Fatalf("queries = %v, want [[88 88]]", got)
	}
	if sink.lastBlock() != 88 {
		t.Fatalf("cursor = %d, want 88", sink.lastBlock())
	}
}

func TestWatcherResumesFromCursor(t *testing.T) {
	src := newFakeSource(100)
	src.logsByBlock[60] = []types.Log{transferLog(60, 0, big.NewInt(7))}
	src.logsByBlock[88] = []types.Log{transferLog(88, 0, big.NewInt(9))}

	sink := &fakeSink{last: 50, has: true}

	w := NewWatcher(Config{Source: src, Sink: sink, Confirmations: 12})
	if err := w.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	if got := src.queryRanges(); len(got) != 1 || got[0] != [2]uint64{51, 88} {
		t.Fatalf("queries = %v, want [[51 88]]", got)
	}

	saved := sink.transfers()
	if len(saved) != 2 || saved[0].BlockNumber != 60 || saved[1].BlockNumber != 88 {
		t.Fatalf("saved = %+v, want two transfers at 60 and 88", saved)
	}
	if sink.lastBlock() != 88 {
		t.Fatalf("cursor = %d, want 88", sink.lastBlock())
	}
}

func TestWatcherAdvancesCursorWithoutTransfers(t *testing.T) {
	src := newFakeSource(100)
	sink := &fakeSink{last: 50, has: true}

	w := NewWatcher(Config{Source: src, Sink: sink, Confirmations: 12})
	if err := w.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	// 这一批没有 Transfer 也必须推进游标，否则下一轮还会重扫同一区间。
	if sink.lastBlock() != 88 {
		t.Fatalf("cursor = %d, want 88", sink.lastBlock())
	}
	if len(sink.transfers()) != 0 {
		t.Fatalf("saved = %v, want none", sink.transfers())
	}
}

func TestWatcherSplitsRangesByBatchSize(t *testing.T) {
	src := newFakeSource(1000)
	sink := &fakeSink{last: 0, has: true}

	w := NewWatcher(Config{Source: src, Sink: sink, Confirmations: 12, BatchSize: 100})
	if err := w.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	if got := src.queryRanges(); len(got) != 1 || got[0] != [2]uint64{1, 100} {
		t.Fatalf("queries = %v, want [[1 100]]", got)
	}

	// 再跑几轮，确认逐批推进直到追上已确认高度。
	for i := 0; i < 10; i++ {
		if err := w.step(context.Background()); err != nil {
			t.Fatalf("step: %v", err)
		}
	}
	if sink.lastBlock() != 988 {
		t.Fatalf("cursor = %d, want 988", sink.lastBlock())
	}
}

func TestWatcherRunRecoversFromSourceError(t *testing.T) {
	src := newFakeSource(100)
	src.headErr = fmt.Errorf("temporary rpc failure")
	sink := &fakeSink{last: 50, has: true}

	w := NewWatcher(Config{
		Source: src, Sink: sink,
		Confirmations: 12,
		PollInterval:  time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// 第一次 BlockNumber 失败，Run 应当退避后重试，最终成功推进游标。
	deadline := time.After(2 * time.Second)
	for sink.lastBlock() != 88 {
		select {
		case <-deadline:
			t.Fatal("watcher did not recover from a transient source error")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// ---------------------------------------------------------------- fakes

type fakeSource struct {
	mu          sync.Mutex
	head        uint64
	headErr     error
	logsByBlock map[uint64][]types.Log
	ranges      [][2]uint64
}

func newFakeSource(head uint64) *fakeSource {
	return &fakeSource{head: head, logsByBlock: map[uint64][]types.Log{}}
}

func (s *fakeSource) BlockNumber(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.headErr; err != nil {
		s.headErr = nil // 只失败一次，模拟瞬时抖动
		return 0, err
	}
	return s.head, nil
}

func (s *fakeSource) FilterLogs(_ context.Context, from, to uint64) ([]types.Log, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ranges = append(s.ranges, [2]uint64{from, to})
	var out []types.Log
	for b := from; b <= to; b++ {
		out = append(out, s.logsByBlock[b]...)
	}
	return out, nil
}

func (s *fakeSource) queryRanges() [][2]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]uint64(nil), s.ranges...)
}

type fakeSink struct {
	mu     sync.Mutex
	last   uint64
	has    bool
	saved  []Transfer
	writes []uint64
}

func (s *fakeSink) LastChainBlock(context.Context) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.has, nil
}

func (s *fakeSink) SaveTransfers(_ context.Context, transfers []Transfer, lastBlock uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, transfers...)
	s.writes = append(s.writes, lastBlock)
	s.last = lastBlock
	s.has = true
	return nil
}

func (s *fakeSink) lastBlock() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *fakeSink) transfers() []Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Transfer(nil), s.saved...)
}
