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
	seq    uint64            // 订单自增序号
	index  map[uint64]*Order // 挂单索引：订单 ID → 订单，撤单时 O(1) 定位
}

// NewEngine 创建撮合引擎。
func NewEngine() *Engine {
	return &Engine{
		book:   NewOrderbook(),
		trades: make([]Trade, 0),
		index:  make(map[uint64]*Order),
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

// ReplayEvent 是回放日志中的一条事件：要么是下单，要么是撤单。
//
// 两种事件必须放在同一个有序序列里回放。如果先回放全部下单、再回放全部撤单，
// 「撤单后又来了新订单」这种顺序会被打乱，撮合结果就与崩溃前不一致。
type ReplayEvent struct {
	OrderID uint64 // 下单事件：新订单 ID；撤单事件：被撤销的订单 ID
	Size    float64
	Price   float64
	Bid     bool
	Cancel  bool
}

// Replay 按给定顺序重新应用历史事件，重建订单簿与成交历史。
//
// 撮合过程是确定性的：订单的到达顺序、价格、数量一旦确定，
// 每一笔成交的对手方和成交量也唯一确定。时间戳只影响同价位内的
// 先后排序，而回放按日志顺序提交，到达顺序本就一致。
// 因此回放结束后，内存状态与进程退出前完全等价。
func (e *Engine) Replay(events []ReplayEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, ev := range events {
		if ev.Cancel {
			// 日志里可能有重复撤单，或者撤的是已被吃掉的订单，忽略即可
			e.cancelLocked(ev.OrderID)
			continue
		}
		if ev.OrderID > e.seq {
			e.seq = ev.OrderID
		}
		e.submitLocked(ev.OrderID, ev.Size, ev.Price, ev.Bid)
	}
}

// HasOrder 判断订单是否仍挂在订单簿上。
//
// 只用于写入撤单日志前做一次前置校验：对不存在的订单先返回 404，
// 而不是先写一条无意义的日志再发现撤不掉。
func (e *Engine) HasOrder(id uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	_, ok := e.index[id]
	return ok
}

// CancelOrder 撤销一笔仍在订单簿上的订单，成功返回 true。
//
// 已完全成交的订单不在索引里，因此也会返回 false：
// 交易所的撤单接口同样不会为已成交订单做任何事。
func (e *Engine) CancelOrder(id uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.cancelLocked(id)
}

// cancelLocked 是 CancelOrder 与回放的共同实现。调用前必须已持有 e.mu。
func (e *Engine) cancelLocked(id uint64) bool {
	o, ok := e.index[id]
	if !ok {
		return false
	}

	limit := o.Limit
	limit.DeleteOrder(o) // 同时扣减档位总量并断开 o 与档位的关系

	// 撤单可能让整个档位变空，空档位必须从订单簿和索引中移除
	if limit.IsEmpty() {
		e.book.RemoveLimit(limit)
	}
	delete(e.index, id)
	return true
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
		e.index[o.ID] = o
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
		// 完全成交的订单不再可撤，必须从撤单索引里移除，
		// 否则索引会一直持有已消失的订单，撤单也会误报成功。
		delete(e.index, counter.ID)
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
