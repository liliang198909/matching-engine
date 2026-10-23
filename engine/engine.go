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
	return e.submitLocked(e.seq, size, price, bid)
}

// SubmitWithID 用调用方指定的 ID 提交订单。
//
// 两种场景会用到它：
//  1. 正常情况下由数据库序列分配 ID，保证重启后编号连续、不与历史订单撞号
//  2. 启动时回放历史订单，ID 直接取自数据库
func (e *Engine) SubmitWithID(id uint64, size, price float64, bid bool) []Trade {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 自增游标至少要越过这次使用的 ID，否则后续 SubmitOrder 会复用编号。
	if id > e.seq {
		e.seq = id
	}
	return e.submitLocked(id, size, price, bid)
}

// ReplayOrder 是回放历史订单所需的最小输入。
//
// 用引擎自己的结构体而不是直接接收数据库的行类型，
// 是为了保持依赖方向：engine 不认识 store，store 也不用认识 engine。
type ReplayOrder struct {
	ID    uint64
	Size  float64
	Price float64
	Bid   bool
}

// Replay 按给定顺序重新提交历史订单，重建订单簿与成交历史。
//
// 撮合过程是确定性的：订单的到达顺序、价格、数量一旦确定，
// 每一笔成交的对手方和成交量也唯一确定。时间戳只影响同价位内的
// 先后排序，而回放按 ID 递增提交，到达顺序本就一致。
// 因此回放结束后，内存状态与进程退出前完全等价。
func (e *Engine) Replay(orders []ReplayOrder) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, o := range orders {
		if o.ID > e.seq {
			e.seq = o.ID
		}
		e.submitLocked(o.ID, o.Size, o.Price, o.Bid)
	}
}

// submitLocked 是 SubmitOrder 与 SubmitWithID 的共同实现。
// 调用前必须已持有 e.mu。
func (e *Engine) submitLocked(id uint64, size, price float64, bid bool) []Trade {
	o := NewOrder(id, size, bid)

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
