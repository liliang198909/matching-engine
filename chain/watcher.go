package chain

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

// Source 是 watcher 对链节点的最小依赖：当前高度 + 按区块区间拉日志。
//
// 抽象成接口而不是直接持有 *ethclient.Client，是为了让测试可以不连链。
// 这是 dependency injection 最常见的用法：依赖「能力」，不依赖「实现」。
type Source interface {
	BlockNumber(ctx context.Context) (uint64, error)
	FilterLogs(ctx context.Context, from, to uint64) ([]types.Log, error)
}

// Sink 是 watcher 对本地存储的要求。
type Sink interface {
	// LastChainBlock 返回已处理到的区块高度；ok 为 false 表示从未处理过。
	LastChainBlock(ctx context.Context) (block uint64, ok bool, err error)
	// SaveTransfers 在同一个事务里写入这批转账，并把游标推进到 lastBlock。
	SaveTransfers(ctx context.Context, transfers []Transfer, lastBlock uint64) error
}

// Config 是 watcher 的运行参数。零值字段会被填上合理默认值。
type Config struct {
	Source        Source
	Sink          Sink
	Confirmations uint64        // 只处理高度 <= head-Confirmations 的区块
	BatchSize     uint64        // 单轮最多回看的区块数
	PollInterval  time.Duration // 两轮之间的间隔
	StartBlock    uint64        // 首次启动的起点；0 表示从当前已确认高度开始跟
}

// Watcher 轮询新区块，解码 ERC20 Transfer 事件并落库。
//
// 为什么用轮询而不是 WebSocket 订阅？
//  1. 订阅要求 wss 端点，很多 RPC 服务只给 https；
//  2. 订阅断线后需要手动补上断线期间的空洞，轮询天生没有这个问题；
//  3. 游标存在库里，进程重启后从断点继续，不需要额外逻辑。
//
// 代价是有秒级的推送延迟，对本项目这种「成交明细落库」的场景完全够用。
// 需要毫秒级推送时才值得上订阅，并且要配套实现断点补齐。
type Watcher struct {
	cfg Config
}

// NewWatcher 构造 watcher 并补齐默认参数。
func NewWatcher(cfg Config) *Watcher {
	if cfg.Confirmations == 0 {
		cfg.Confirmations = 12
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 2000
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	return &Watcher{cfg: cfg}
}

// Run 持续跟进链上事件，直到 ctx 被取消。
//
// 单个轮次失败不会终止：RPC 抖动、节点切换、数据库短暂不可用都属于常态，
// 这里用指数退避重试，最长间隔 30 秒。已成功的轮次会把退避重置回 1 秒，
// 避免偶发错误之后一直按最长间隔轮询。
func (w *Watcher) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if err := w.step(ctx); err != nil {
			log.Printf("chain watcher: %v (retry in %s)", err, backoff)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		backoff = time.Second
		if !sleep(ctx, w.cfg.PollInterval) {
			return
		}
	}
}

// step 处理一批已确认的区块。没有新块时直接返回，不做任何写入。
func (w *Watcher) step(ctx context.Context) error {
	head, err := w.cfg.Source.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("read head: %w", err)
	}

	// 确认数以内的区块可能被重组，先不处理。
	// 这是最省事也最有效的重组防护：等 12 个块之后，短程重组几乎不可能再影响它。
	if head <= w.cfg.Confirmations {
		return nil
	}
	safe := head - w.cfg.Confirmations

	last, ok, err := w.cfg.Sink.LastChainBlock(ctx)
	if err != nil {
		return fmt.Errorf("read cursor: %w", err)
	}

	next := w.cfg.StartBlock
	if ok {
		next = last + 1
	} else if next == 0 {
		// 没有指定起点时只跟新块，避免第一次启动就从创世块扫起。
		next = safe
	}

	if next > safe {
		return nil
	}

	to := next + w.cfg.BatchSize - 1
	if to > safe {
		to = safe
	}

	logs, err := w.cfg.Source.FilterLogs(ctx, next, to)
	if err != nil {
		return fmt.Errorf("filter logs %d-%d: %w", next, to, err)
	}

	transfers := make([]Transfer, 0, len(logs))
	for _, l := range logs {
		if t, isTransfer := DecodeTransfer(l); isTransfer {
			transfers = append(transfers, t)
		}
	}

	// 即使这批没有 Transfer，也要推进游标：否则下一次还会重复扫描同一区间。
	if err := w.cfg.Sink.SaveTransfers(ctx, transfers, to); err != nil {
		return fmt.Errorf("save transfers %d-%d: %w", next, to, err)
	}
	return nil
}

// sleep 等待 d，ctx 取消时立即返回 false。
//
// 不能直接 time.Sleep：那样进程退出时还要等满一个轮询间隔。
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
