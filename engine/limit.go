package engine

// Limit 表示一个价格档位，聚合同一价格上的所有订单。
type Limit struct {
	Price       float64  // 档位价格
	Bid         bool     // true 为买盘档位，false 为卖盘档位
	Orders      []*Order // 档位内订单，按到达时间先后排列（时间优先）
	TotalVolume float64  // 档位内剩余未成交总量
}

// NewLimit 创建一个空的价格档位。
func NewLimit(price float64, bid bool) *Limit {
	return &Limit{
		Price:  price,
		Bid:    bid,
		Orders: make([]*Order, 0),
	}
}

// AddOrder 将订单追加到档位尾部，保证同价位下先到者先成交。
func (l *Limit) AddOrder(o *Order) {
	o.Limit = l
	l.Orders = append(l.Orders, o)
	l.TotalVolume += o.Size
}

// DeleteOrder 从档位中移除订单，并解除其反向引用。
//
// 这里采用保序删除（O(n)）而非「与尾部元素交换」（O(1)）：
// 之所以牺牲常数级性能，是为了维持队列的 FIFO 语义——
// 交换会打乱到达顺序，破坏「时间优先」这一撮合核心规则。
func (l *Limit) DeleteOrder(o *Order) {
	for i, order := range l.Orders {
		if order != o {
			continue
		}
		l.Orders = append(l.Orders[:i], l.Orders[i+1:]...)
		l.TotalVolume -= order.Size
		o.Limit = nil
		return
	}
}

// IsEmpty 判断档位内是否已无剩余订单。
func (l *Limit) IsEmpty() bool {
	return len(l.Orders) == 0
}
