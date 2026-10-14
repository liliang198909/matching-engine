package engine

import "testing"

func TestLimitAddOrder(t *testing.T) {
	l := NewLimit(100, true)

	a := NewOrder(1, 5, true)
	b := NewOrder(2, 8, true)
	l.AddOrder(a)
	l.AddOrder(b)

	if l.TotalVolume != 13 {
		t.Fatalf("TotalVolume = %v, 期望 13", l.TotalVolume)
	}
	if len(l.Orders) != 2 {
		t.Fatalf("len(Orders) = %d, 期望 2", len(l.Orders))
	}
	if a.Limit != l {
		t.Error("订单未正确建立档位反向引用")
	}
}

func TestLimitDeleteOrderKeepsFIFO(t *testing.T) {
	l := NewLimit(100, true)

	a := NewOrder(1, 5, true)
	b := NewOrder(2, 8, true)
	c := NewOrder(3, 10, true)
	l.AddOrder(a)
	l.AddOrder(b)
	l.AddOrder(c)

	l.DeleteOrder(b)

	if l.TotalVolume != 15 {
		t.Fatalf("TotalVolume = %v, 期望 15", l.TotalVolume)
	}
	if len(l.Orders) != 2 {
		t.Fatalf("len(Orders) = %d, 期望 2", len(l.Orders))
	}
	// 保序删除：剩余订单必须维持原有到达顺序
	if l.Orders[0] != a || l.Orders[1] != c {
		t.Error("删除后队列顺序被破坏，时间优先语义失效")
	}
	if b.Limit != nil {
		t.Error("被删订单的反向引用未清空")
	}
}

func TestLimitDeleteUnknownOrder(t *testing.T) {
	l := NewLimit(100, true)
	l.AddOrder(NewOrder(1, 5, true))

	// 删除不存在的订单不应影响档位状态
	l.DeleteOrder(NewOrder(99, 3, true))

	if l.TotalVolume != 5 || len(l.Orders) != 1 {
		t.Fatalf("删除未知订单后档位被误改：volume=%v orders=%d", l.TotalVolume, len(l.Orders))
	}
}

func TestOrderbookSorting(t *testing.T) {
	ob := NewOrderbook()

	// 故意乱序挂入，验证排序逻辑
	ob.PlaceOrder(99, NewOrder(1, 1, true))
	ob.PlaceOrder(101, NewOrder(2, 1, true))
	ob.PlaceOrder(100, NewOrder(3, 1, true))

	ob.PlaceOrder(103, NewOrder(4, 1, false))
	ob.PlaceOrder(101, NewOrder(5, 1, false))
	ob.PlaceOrder(102, NewOrder(6, 1, false))

	wantBids := []float64{101, 100, 99}
	wantAsks := []float64{101, 102, 103}

	for i, want := range wantBids {
		if ob.Bids[i].Price != want {
			t.Errorf("买盘第 %d 档 = %v, 期望 %v（买盘应为降序）", i, ob.Bids[i].Price, want)
		}
	}
	for i, want := range wantAsks {
		if ob.Asks[i].Price != want {
			t.Errorf("卖盘第 %d 档 = %v, 期望 %v（卖盘应为升序）", i, ob.Asks[i].Price, want)
		}
	}
	if len(ob.BidLimits) != 3 || len(ob.AskLimits) != 3 {
		t.Errorf("价格索引数量与切片不一致：bids=%d asks=%d", len(ob.BidLimits), len(ob.AskLimits))
	}
}

func TestOrderbookReusesExistingLimit(t *testing.T) {
	ob := NewOrderbook()
	ob.PlaceOrder(100, NewOrder(1, 5, true))
	ob.PlaceOrder(100, NewOrder(2, 7, true))

	if len(ob.Bids) != 1 {
		t.Fatalf("同价位应复用档位，实际切片长度 = %d", len(ob.Bids))
	}
	if ob.Bids[0].TotalVolume != 12 {
		t.Fatalf("档位总量 = %v, 期望 12", ob.Bids[0].TotalVolume)
	}
}

func TestBestPriceOnEmptyBook(t *testing.T) {
	ob := NewOrderbook()

	if ob.BestBid() != nil || ob.BestAsk() != nil {
		t.Fatal("空订单簿的最优价应为 nil")
	}
}

func TestBestPrice(t *testing.T) {
	ob := NewOrderbook()
	ob.PlaceOrder(100, NewOrder(1, 1, true))
	ob.PlaceOrder(105, NewOrder(2, 1, false))
	ob.PlaceOrder(98, NewOrder(3, 1, true))
	ob.PlaceOrder(107, NewOrder(4, 1, false))

	if got := ob.BestBid().Price; got != 100 {
		t.Errorf("最优买价 = %v, 期望 100", got)
	}
	if got := ob.BestAsk().Price; got != 105 {
		t.Errorf("最优卖价 = %v, 期望 105", got)
	}
}

func TestRemoveLimit(t *testing.T) {
	ob := NewOrderbook()
	ob.PlaceOrder(100, NewOrder(1, 1, true))

	l := ob.BestBid()
	ob.RemoveLimit(l)

	if len(ob.Bids) != 0 {
		t.Errorf("移除后买盘切片长度 = %d, 期望 0", len(ob.Bids))
	}
	if _, ok := ob.BidLimits[100]; ok {
		t.Error("移除档位后价格索引未清理")
	}
	if ob.BestBid() != nil {
		t.Error("移除唯一档位后最优买价应为 nil")
	}
}
