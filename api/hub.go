package api

import (
	"encoding/json"
	"sync"
)

// Hub 把引擎状态变化广播给所有 WebSocket 订阅者。
//
// 它是一个极简的扇出器：每个订阅者拿到一个有界缓冲通道，
// 消息在发布时序列化一次，再分发给所有人，避免 N 个连接重复编码。
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

// NewHub 创建一个空的广播中心。
func NewHub() *Hub {
	return &Hub{clients: make(map[chan []byte]struct{})}
}

// Subscribe 注册一个订阅者，返回消息通道与取消函数。
//
// 通道有缓冲，目的是让撮合路径上的 Publish 永远不会因为某个慢客户端
// 而阻塞——交易所的主循环绝不能等网络。
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 16)

	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		// 用 delete 的返回值判断是否已注销，保证 close 只执行一次
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
	}
	return ch, cancel
}

// Publish 把一条消息序列化后发给所有订阅者。
//
// 发送是非阻塞的：某个连接卡住时丢弃这一条，而不是拖慢整个服务。
// 丢弃是可以接受的——depth 是快照，下一条就会覆盖；trade 丢的也只是推送，
// 历史成交仍然可以用 GET /trades 补齐。
func (h *Hub) Publish(payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		// 内部的固定 DTO 结构不会序列化失败，真失败说明是编程错误，直接放弃这条
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.clients {
		select {
		case ch <- data:
		default:
			// 缓冲已满：这个客户端跟不上，跳过本条
		}
	}
}

// clientsCount 返回当前订阅者数量，仅供测试与排查使用。
func (h *Hub) clientsCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}
