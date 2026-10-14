package engine

import (
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
