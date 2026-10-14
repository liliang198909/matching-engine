package engine

import "time"

// Order 表示一笔限价委托订单。
type Order struct {
	ID        uint64  // 订单唯一标识
	Size      float64 // 剩余未成交数量
	Bid       bool    // true 为买单，false 为卖单
	Timestamp int64   // 下单时间（Unix 纳秒），用于同价位的时间优先排序
	Limit     *Limit  // 所属价格档位；完全成交或撤单后置为 nil
}

// NewOrder 创建一笔订单。
func NewOrder(id uint64, size float64, bid bool) *Order {
	return &Order{
		ID:        id,
		Size:      size,
		Bid:       bid,
		Timestamp: time.Now().UnixNano(),
	}
}

// IsFilled 判断订单是否已全部成交。
func (o *Order) IsFilled() bool {
	return o.Size <= 0
}
