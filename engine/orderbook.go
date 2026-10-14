package engine

import "sort"

// Orderbook 订单簿，分别维护买盘与卖盘。
//
// 采用「有序切片 + 哈希表」双索引结构：
//   - 切片按价格排序，撮合时取首元素即为最优价，无需每次全量扫描
//   - 哈希表以价格为键，档位查找与插入为 O(1)
//
// 两套索引指向同一批 Limit 对象，因此必须是同一个实例持有，
// 外部只应通过本类型的方法操作订单簿，避免索引失配。
type Orderbook struct {
	Bids      []*Limit           // 买盘，按价格从高到低
	Asks      []*Limit           // 卖盘，按价格从低到高
	BidLimits map[float64]*Limit // 买盘价格索引
	AskLimits map[float64]*Limit // 卖盘价格索引
}

// NewOrderbook 创建一个空订单簿。
func NewOrderbook() *Orderbook {
	return &Orderbook{
		BidLimits: make(map[float64]*Limit),
		AskLimits: make(map[float64]*Limit),
	}
}

// PlaceOrder 将订单挂入订单簿；目标价位尚无档位时会先创建档位。
func (ob *Orderbook) PlaceOrder(price float64, o *Order) {
	ob.limit(price, o.Bid).AddOrder(o)
}

// limit 返回指定价格的档位，不存在则创建并插入有序切片。
func (ob *Orderbook) limit(price float64, bid bool) *Limit {
	if bid {
		if l, ok := ob.BidLimits[price]; ok {
			return l
		}
		l := NewLimit(price, true)
		ob.BidLimits[price] = l
		ob.Bids = append(ob.Bids, l)
		ob.sortBids()
		return l
	}

	if l, ok := ob.AskLimits[price]; ok {
		return l
	}
	l := NewLimit(price, false)
	ob.AskLimits[price] = l
	ob.Asks = append(ob.Asks, l)
	ob.sortAsks()
	return l
}

// sortBids 维持买盘价格降序：出价高者优先成交。
func (ob *Orderbook) sortBids() {
	sort.Slice(ob.Bids, func(i, j int) bool {
		return ob.Bids[i].Price > ob.Bids[j].Price
	})
}

// sortAsks 维持卖盘价格升序：要价低者优先成交。
func (ob *Orderbook) sortAsks() {
	sort.Slice(ob.Asks, func(i, j int) bool {
		return ob.Asks[i].Price < ob.Asks[j].Price
	})
}

// RemoveLimit 移除已清空的档位，避免空档位长期占用内存与索引。
func (ob *Orderbook) RemoveLimit(l *Limit) {
	if l.Bid {
		delete(ob.BidLimits, l.Price)
		ob.Bids = removeLimit(ob.Bids, l)
		return
	}
	delete(ob.AskLimits, l.Price)
	ob.Asks = removeLimit(ob.Asks, l)
}

func removeLimit(limits []*Limit, target *Limit) []*Limit {
	for i, l := range limits {
		if l == target {
			return append(limits[:i], limits[i+1:]...)
		}
	}
	return limits
}

// BestBid 返回最优买价档位；买盘为空时返回 nil。
func (ob *Orderbook) BestBid() *Limit {
	if len(ob.Bids) == 0 {
		return nil
	}
	return ob.Bids[0]
}

// BestAsk 返回最优卖价档位；卖盘为空时返回 nil。
func (ob *Orderbook) BestAsk() *Limit {
	if len(ob.Asks) == 0 {
		return nil
	}
	return ob.Asks[0]
}
