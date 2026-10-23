package engine

import (
	"reflect"
	"sync"
	"testing"
)

func TestNoCrossWhenPricesDoNotOverlap(t *testing.T) {
	e := NewEngine()

	if got := e.SubmitOrder(5, 101, false); len(got) != 0 {
		t.Fatalf("首笔挂单不应产生成交，却得到 %v", got)
	}
	// 买单出价低于卖单要价，无法成交
	if got := e.SubmitOrder(5, 100, true); len(got) != 0 {
		t.Fatalf("价格未交叉不应成交，却得到 %v", got)
	}

	bids, asks := e.Depth(5)
	if len(bids) != 1 || bids[0].Price != 100 || bids[0].Volume != 5 {
		t.Errorf("买盘 = %v, 期望 [{100 5}]", bids)
	}
	if len(asks) != 1 || asks[0].Price != 101 || asks[0].Volume != 5 {
		t.Errorf("卖盘 = %v, 期望 [{101 5}]", asks)
	}
}

func TestFullMatch(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(5, 100, false) // 卖 5 @ 100

	trades := e.SubmitOrder(5, 100, true) // 买 5 @ 100

	if len(trades) != 1 {
		t.Fatalf("成交笔数 = %d, 期望 1", len(trades))
	}
	if trades[0].Size != 5 {
		t.Errorf("成交量 = %v, 期望 5", trades[0].Size)
	}
	if trades[0].Price != 100 {
		t.Errorf("成交价 = %v, 期望 100", trades[0].Price)
	}
	if trades[0].BidOrder != 2 || trades[0].AskOrder != 1 {
		t.Errorf("成交双方 ID = (%d, %d), 期望 (2, 1)", trades[0].BidOrder, trades[0].AskOrder)
	}

	bids, asks := e.Depth(5)
	if len(bids) != 0 || len(asks) != 0 {
		t.Errorf("完全成交后订单簿应为空，实际 bids=%v asks=%v", bids, asks)
	}
	if got := len(e.Trades()); got != 1 {
		t.Errorf("历史成交记录数 = %d, 期望 1", got)
	}
}

func TestPartialFill(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(10, 100, false) // 卖 10 @ 100

	trades := e.SubmitOrder(4, 100, true) // 买 4 @ 100
	if len(trades) != 1 || trades[0].Size != 4 {
		t.Fatalf("成交量应为 4，实际 %v", trades)
	}

	_, asks := e.Depth(5)
	if len(asks) != 1 || asks[0].Volume != 6 {
		t.Fatalf("卖盘剩余量应为 6，实际 %v", asks)
	}

	// 后续买单吃掉剩余部分，档位应被完全清理
	trades = e.SubmitOrder(6, 100, true)
	if len(trades) != 1 || trades[0].Size != 6 {
		t.Fatalf("第二笔成交量应为 6，实际 %v", trades)
	}

	bids, asks := e.Depth(5)
	if len(asks) != 0 || len(bids) != 0 {
		t.Errorf("档位吃空后应被移除，实际 bids=%v asks=%v", bids, asks)
	}
}

func TestPricePriority(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(5, 100, false) // 更优卖价
	e.SubmitOrder(5, 101, false)

	trades := e.SubmitOrder(10, 101, true) // 买 10 @ 101，应依次吃掉两个档位

	if len(trades) != 2 {
		t.Fatalf("成交笔数 = %d, 期望 2", len(trades))
	}
	if trades[0].Price != 100 || trades[0].Size != 5 {
		t.Errorf("首笔应先与更优价成交，实际 %v", trades[0])
	}
	if trades[1].Price != 101 || trades[1].Size != 5 {
		t.Errorf("次笔应成交剩余档位，实际 %v", trades[1])
	}
}

func TestTimePriority(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(5, 100, false) // 先到，ID = 1
	e.SubmitOrder(5, 100, false) // 后到，ID = 2

	trades := e.SubmitOrder(5, 100, true) // 买 5，应与先到者成交

	if len(trades) != 1 {
		t.Fatalf("成交笔数 = %d, 期望 1", len(trades))
	}
	if trades[0].AskOrder != 1 {
		t.Fatalf("应与先到订单成交，实际对手单 ID = %d", trades[0].AskOrder)
	}

	_, asks := e.Depth(5)
	if len(asks) != 1 || asks[0].Volume != 5 {
		t.Errorf("后到订单应残留 5，实际 %v", asks)
	}
}

func TestSellCrossesRestingBids(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(3, 100, true) // 买 3 @ 100
	e.SubmitOrder(3, 98, true)  // 买 3 @ 98

	// 卖 4 @ 98：先吃 100 的档位，再吃 98 的档位
	trades := e.SubmitOrder(4, 98, false)

	if len(trades) != 2 {
		t.Fatalf("成交笔数 = %d, 期望 2", len(trades))
	}
	if trades[0].Price != 100 || trades[0].Size != 3 {
		t.Errorf("应先与最优买价成交，实际 %v", trades[0])
	}
	if trades[1].Price != 98 || trades[1].Size != 1 {
		t.Errorf("再与次优买价成交 1 手，实际 %v", trades[1])
	}
}

func TestDepth(t *testing.T) {
	e := NewEngine()

	for i := 0; i < 5; i++ {
		e.SubmitOrder(1, float64(100+i), true)  // 买盘 100..104
		e.SubmitOrder(1, float64(200+i), false) // 卖盘 200..204
	}

	bids, asks := e.Depth(3)

	if len(bids) != 3 || len(asks) != 3 {
		t.Fatalf("深度档数 = (%d, %d), 期望 (3, 3)", len(bids), len(asks))
	}
	wantBids := []float64{104, 103, 102}
	wantAsks := []float64{200, 201, 202}

	for i, want := range wantBids {
		if bids[i].Price != want {
			t.Errorf("买盘第 %d 档 = %v, 期望 %v", i, bids[i].Price, want)
		}
	}
	for i, want := range wantAsks {
		if asks[i].Price != want {
			t.Errorf("卖盘第 %d 档 = %v, 期望 %v", i, asks[i].Price, want)
		}
	}
}

func TestDepthLimitZero(t *testing.T) {
	e := NewEngine()
	e.SubmitOrder(1, 100, true)

	bids, asks := e.Depth(0)
	if len(bids) != 0 || len(asks) != 0 {
		t.Errorf("n = 0 时不应返回任何档位，实际 bids=%v asks=%v", bids, asks)
	}
}

func TestVolumeConsistency(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(10, 100, true)
	e.SubmitOrder(7, 99, true)
	e.SubmitOrder(4, 101, false)

	trades := e.SubmitOrder(6, 101, true) // 吃掉落单的 4 手卖单

	if len(trades) != 1 || trades[0].Size != 4 {
		t.Errorf("应与卖单成交 4 手，实际 %v", trades)
	}

	// 盘口每档总量都应为正数，且与档位内订单剩余量之和一致
	bids, asks := e.Depth(10)
	for _, lvl := range bids {
		if lvl.Volume <= 0 {
			t.Errorf("买盘档位 %v 总量不应为 0 或负数", lvl.Price)
		}
	}
	for _, lvl := range asks {
		if lvl.Volume <= 0 {
			t.Errorf("卖盘档位 %v 总量不应为 0 或负数", lvl.Price)
		}
	}
}

func TestConcurrentSubmit(t *testing.T) {
	e := NewEngine()

	const n = 100
	var wg sync.WaitGroup

	// 半数买单、半数卖单并发提交，价格相同、数量各 1
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				e.SubmitOrder(1, 100, true)
				return
			}
			e.SubmitOrder(1, 100, false)
		}(i)
	}
	wg.Wait()

	// 50 手买对 50 手卖，应全部撮合成 50 笔
	if got := len(e.Trades()); got != 50 {
		t.Fatalf("成交笔数 = %d, 期望 50", got)
	}

	bids, asks := e.Depth(10)
	if len(bids) != 0 || len(asks) != 0 {
		t.Errorf("数量完全匹配后订单簿应为空，实际 bids=%v asks=%v", bids, asks)
	}
}

// TestReplayRebuildsState 验证「订单日志回放」能把状态复原。
//
// 这是持久化的正确性基础：只要回放后的订单簿和成交历史与崩溃前一致，
// 数据库里就只需要存订单，不必再单独存一份成交记录。
func TestReplayRebuildsState(t *testing.T) {
	orig := NewEngine()

	// 覆盖挂单、部分成交、跨越多个档位扫单三种情况
	orig.SubmitOrder(5, 101, false)
	orig.SubmitOrder(3, 100, false)
	orig.SubmitOrder(4, 102, false)
	orig.SubmitOrder(6, 101, true) // 吃掉 100×3，再吃 101×2
	orig.SubmitOrder(2, 99, true)  // 买价低于卖价，挂入买盘

	wantTrades := orig.Trades()
	wantBids, wantAsks := orig.Depth(5)

	replay := []ReplayEvent{
		{OrderID: 1, Size: 5, Price: 101, Bid: false},
		{OrderID: 2, Size: 3, Price: 100, Bid: false},
		{OrderID: 3, Size: 4, Price: 102, Bid: false},
		{OrderID: 4, Size: 6, Price: 101, Bid: true},
		{OrderID: 5, Size: 2, Price: 99, Bid: true},
	}

	restored := NewEngine()
	restored.Replay(replay)

	if got := restored.Trades(); !reflect.DeepEqual(got, wantTrades) {
		t.Errorf("回放后成交历史 = %v, 期望 %v", got, wantTrades)
	}
	gotBids, gotAsks := restored.Depth(5)
	if !reflect.DeepEqual(gotBids, wantBids) || !reflect.DeepEqual(gotAsks, wantAsks) {
		t.Errorf("回放后盘口 = bids %v asks %v, 期望 bids %v asks %v",
			gotBids, gotAsks, wantBids, wantAsks)
	}

	// 回放结束后新订单必须接着最大历史 ID，不能从 1 重新开始
	trades := restored.SubmitOrder(1, 99, false)
	if len(trades) != 1 {
		t.Fatalf("成交笔数 = %d, 期望 1", len(trades))
	}
	if trades[0].AskOrder != 6 || trades[0].BidOrder != 5 {
		t.Errorf("新订单 ID = %d, 期望 6；对手方 = %d, 期望 5",
			trades[0].AskOrder, trades[0].BidOrder)
	}
}

func TestCancelRemovesOrderAndEmptyLevel(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(5, 100, false) // ID 1
	e.SubmitOrder(3, 101, false) // ID 2

	if !e.HasOrder(1) {
		t.Fatal("订单 1 应在订单簿上")
	}
	if !e.CancelOrder(1) {
		t.Fatal("撤单应成功")
	}

	// 100 档位只剩这一笔，撤掉后整个档位也应消失
	bids, asks := e.Depth(5)
	if len(bids) != 0 {
		t.Errorf("买盘应为空，实际 %v", bids)
	}
	if len(asks) != 1 || asks[0].Price != 101 || asks[0].Volume != 3 {
		t.Errorf("卖盘 = %v, 期望 [{101 3}]", asks)
	}
	if e.HasOrder(1) || e.CancelOrder(1) {
		t.Error("已撤销的订单不应还能再次撤销")
	}
}

func TestCancelKeepsFIFOOfRemainingOrders(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(2, 100, false) // ID 1
	e.SubmitOrder(4, 100, false) // ID 2
	e.SubmitOrder(3, 100, false) // ID 3

	if !e.CancelOrder(2) {
		t.Fatal("撤单应成功")
	}

	// 撤掉中间那笔后，剩余订单的先后顺序不能被破坏
	trades := e.SubmitOrder(5, 100, true)
	if len(trades) != 2 {
		t.Fatalf("成交笔数 = %d, 期望 2", len(trades))
	}
	if trades[0].AskOrder != 1 || trades[0].Size != 2 {
		t.Errorf("第一笔 = %+v, 期望先吃订单 1 的 2", trades[0])
	}
	if trades[1].AskOrder != 3 || trades[1].Size != 3 {
		t.Errorf("第二笔 = %+v, 期望再吃订单 3 的 3", trades[1])
	}
}

func TestCancelRejectsFilledOrder(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(5, 100, false) // ID 1
	e.SubmitOrder(5, 100, true)  // ID 2，完全吃掉订单 1
	if e.CancelOrder(1) || e.HasOrder(1) {
		t.Error("已成交的订单不应还能撤销")
	}
	if e.CancelOrder(999) || e.HasOrder(999) {
		t.Error("不存在的订单不应能撤销")
	}
}

// TestReplayAppliesCancellationsInOrder 验证撤单与下单必须按日志原序回放。
//
// 若把所有撤单挪到最后统一处理，下面的买单会先吃到那笔被撤销的 100 卖单，
// 成交结果与真实进程完全不同。
func TestReplayAppliesCancellationsInOrder(t *testing.T) {
	orig := NewEngine()

	orig.SubmitOrder(5, 100, false) // ID 1，随后被撤
	orig.SubmitOrder(3, 101, false) // ID 2
	orig.CancelOrder(1)
	orig.SubmitOrder(4, 101, true) // ID 3，只能吃到 101 档

	wantTrades := orig.Trades()
	wantBids, wantAsks := orig.Depth(5)

	restored := NewEngine()
	restored.Replay([]ReplayEvent{
		{OrderID: 1, Size: 5, Price: 100},
		{OrderID: 2, Size: 3, Price: 101},
		{OrderID: 1, Cancel: true},
		{OrderID: 3, Size: 4, Price: 101, Bid: true},
	})

	if got := restored.Trades(); !reflect.DeepEqual(got, wantTrades) {
		t.Errorf("回放后成交历史 = %v, 期望 %v", got, wantTrades)
	}
	gotBids, gotAsks := restored.Depth(5)
	if !reflect.DeepEqual(gotBids, wantBids) || !reflect.DeepEqual(gotAsks, wantAsks) {
		t.Errorf("回放后盘口 = bids %v asks %v, 期望 bids %v asks %v",
			gotBids, gotAsks, wantBids, wantAsks)
	}

	// 已撤销订单的状态也必须能查出来，而不是被当成不存在
	if st, ok := restored.Status(1); !ok || st.State != StateCancelled {
		t.Errorf("订单 1 状态 = %+v ok=%v, 期望 cancelled", st, ok)
	}
	if st, ok := restored.Status(3); !ok || st.State != StateOpen || st.Remaining != 1 {
		t.Errorf("订单 3 状态 = %+v ok=%v, 期望 open 且剩余 1", st, ok)
	}
}

// TestOrderStatusLifecycle 覆盖 open → 部分成交 → 撤单、以及完全成交两条路径。
func TestOrderStatusLifecycle(t *testing.T) {
	e := NewEngine()

	e.SubmitOrder(5, 100, false) // ID 1，卖 5 @ 100

	st, ok := e.Status(1)
	if !ok {
		t.Fatal("订单 1 应可查询")
	}
	if st.State != StateOpen || st.Size != 5 || st.Filled != 0 || st.Remaining != 5 {
		t.Errorf("新订单状态 = %+v, 期望 open 且全部未成交", st)
	}
	if st.Price != 100 || st.Bid {
		t.Errorf("订单价格/方向 = %v/%v, 期望 100 卖单", st.Price, st.Bid)
	}

	e.SubmitOrder(3, 100, true) // ID 2，成交 3

	// 被动方订单部分成交：数量减少，但状态仍是 open
	if st, _ := e.Status(1); st.Filled != 3 || st.Remaining != 2 || st.State != StateOpen {
		t.Errorf("部分成交后订单 1 = %+v, 期望 filled=3 remaining=2 open", st)
	}
	// 主动方订单被完全吃掉
	if st, _ := e.Status(2); st.State != StateFilled || st.Filled != 3 || st.Remaining != 0 {
		t.Errorf("完全成交后订单 2 = %+v, 期望 filled", st)
	}

	e.CancelOrder(1)

	// 撤单只撤剩余量，已成交部分保留在记录里
	if st, _ := e.Status(1); st.State != StateCancelled || st.Remaining != 0 || st.Filled != 3 {
		t.Errorf("撤单后订单 1 = %+v, 期望 cancelled 且 filled=3", st)
	}

	if _, ok := e.Status(999); ok {
		t.Error("不存在的订单不应可查询")
	}
}

func TestTradePage(t *testing.T) {
	e := NewEngine()

	// 三对完全成交的订单，各产生一笔成交，价格依次为 100、101、102
	for _, price := range []float64{100, 101, 102} {
		e.SubmitOrder(5, price, false)
		e.SubmitOrder(5, price, true)
	}

	trades, total := e.TradePage(0, 10)
	if total != 3 || len(trades) != 3 {
		t.Fatalf("全量 = %d 笔 / total %d，期望 3 笔", len(trades), total)
	}

	// 窗口按产生顺序切分：offset=1 从第二笔开始
	page, total := e.TradePage(1, 2)
	if total != 3 || len(page) != 2 {
		t.Fatalf("窗口 = %d 笔 / total %d，期望 2 笔", len(page), total)
	}
	if page[0].Price != 101 || page[1].Price != 102 {
		t.Errorf("窗口价格 = %v/%v，期望 101/102", page[0].Price, page[1].Price)
	}

	// 越界返回空切片而不是报错，且不能破坏已有历史
	if page, total := e.TradePage(99, 10); len(page) != 0 || total != 3 {
		t.Errorf("越界窗口 = %d 笔 / total %d，期望 0 笔", len(page), total)
	}
	if all := e.Trades(); len(all) != 3 {
		t.Errorf("分页后历史 = %d 笔，期望仍为 3", len(all))
	}
}
