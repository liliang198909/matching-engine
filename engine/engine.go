package engine

import "sync"

// Trade 表示一笔成交记录。
type Trade struct {
	Price    float64 // 成交价
	Size     float64 // 成交量
	BidOrder uint64  // 买方订单 ID
	AskOrder uint64  // 卖方订单 ID
}

// PriceLevel 表示盘口的一档价格及其挂单总量。
type PriceLevel struct {
	Price  float64
	Volume float64
}

// Engine 撮合引擎。
//
// 订单簿与成交记录均为共享状态，外部可能从多个 goroutine 并发提交订单，
// 因此所有读写路径统一由互斥锁串行化，保证数据一致性。
type Engine struct {
	mu     sync.Mutex
	book   *Orderbook
	trades []Trade
	seq    uint64 // 订单自增序号
}

// NewEngine 创建撮合引擎。
func NewEngine() *Engine {
	return &Engine{
		book:   NewOrderbook(),
		trades: make([]Trade, 0),
	}
}

// SubmitOrder 提交一笔限价单，返回本次产生的成交明细。
//
// 撮合遵循「价格优先、时间优先」：
//  1. 持续与对手盘最优档位中最早到达的订单撮合，直到价格不再满足或数量耗尽
//  2. 成交价取被动方（挂单方）价格，这也是行业通行做法
//  3. 未成交的剩余数量挂入订单簿等待后续对手单
func (e *Engine) SubmitOrder(size, price float64, bid bool) []Trade {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.seq++
	o := NewOrder(e.seq, size, bid)

	var filled []Trade
	if bid {
		filled = e.matchBid(o, price)
	} else {
		filled = e.matchAsk(o, price)
	}

	// 仍有剩余则挂单，成为后续订单的对手盘
	if !o.IsFilled() {
		e.book.PlaceOrder(price, o)
	}

	e.trades = append(e.trades, filled...)
	return filled
}

// matchBid 让新买单与卖盘最优档位持续撮合。
func (e *Engine) matchBid(o *Order, price float64) []Trade {
	var trades []Trade

	for !o.IsFilled() {
		best := e.book.BestAsk()
		// 卖盘为空，或最优卖价高于买价，无法继续成交
		if best == nil || best.Price > price {
			break
		}

		trade, exhausted := e.fill(o, best.Orders[0], best.Price)
		trades = append(trades, trade)

		// 对手档位被吃空后立即移除，保持订单簿整洁
		if exhausted && best.IsEmpty() {
			e.book.RemoveLimit(best)
		}
	}

	return trades
}

// matchAsk 让新卖单与买盘最优档位持续撮合。
func (e *Engine) matchAsk(o *Order, price float64) []Trade {
	var trades []Trade

	for !o.IsFilled() {
		best := e.book.BestBid()
		// 买盘为空，或最优买价低于卖价，无法继续成交
		if best == nil || best.Price < price {
			break
		}

		trade, exhausted := e.fill(o, best.Orders[0], best.Price)
		trades = append(trades, trade)

		if exhausted && best.IsEmpty() {
			e.book.RemoveLimit(best)
		}
	}

	return trades
}

// fill 撮合新订单与新到订单的对手方，成交量为两者剩余量的较小值。
// 返回成交记录，以及对手订单是否已完全成交。
func (e *Engine) fill(o, counter *Order, price float64) (Trade, bool) {
	size := min(o.Size, counter.Size)

	o.Size -= size
	counter.Size -= size
	counter.Limit.TotalVolume -= size

	trade := Trade{Price: price, Size: size}
	if o.Bid {
		trade.BidOrder, trade.AskOrder = o.ID, counter.ID
	} else {
		trade.BidOrder, trade.AskOrder = counter.ID, o.ID
	}

	exhausted := counter.IsFilled()
	if exhausted {
		counter.Limit.DeleteOrder(counter)
	}
	return trade, exhausted
}

// Trades 返回历史成交记录的副本快照。
func (e *Engine) Trades() []Trade {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]Trade, len(e.trades))
	copy(out, e.trades)
	return out
}

// Depth 返回盘口深度快照，分别给出买盘与卖盘的前 n 档。
func (e *Engine) Depth(n int) (bids, asks []PriceLevel) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i, l := range e.book.Bids {
		if i >= n {
			break
		}
		bids = append(bids, PriceLevel{Price: l.Price, Volume: l.TotalVolume})
	}
	for i, l := range e.book.Asks {
		if i >= n {
			break
		}
		asks = append(asks, PriceLevel{Price: l.Price, Volume: l.TotalVolume})
	}
	return bids, asks
}
