package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/liliang198909/matching-engine/engine"
)

const (
	// wsDepthLevels 是每帧推送的盘口档位数。实时推送只给最活跃的几档，
	// 要看全量深度或历史成交，仍然走 REST 接口。
	wsDepthLevels = 20
	// wsWriteTimeout 防止某个客户端只连不读，把写操作永久卡住。
	wsWriteTimeout = 5 * time.Second
	// wsHandshakeTimeout 限制握手阶段的最长等待时间。
	wsHandshakeTimeout = 5 * time.Second
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: wsHandshakeTimeout,
	// 这是公开演示服务，既没有 cookie 也没有鉴权，允许任意 Origin 不会
	// 带来跨站冒用身份的问题。若将来接入鉴权，这里必须收紧成白名单。
	CheckOrigin: func(*http.Request) bool { return true },
}

// stream 处理 GET /ws：把连接升级为 WebSocket，先发一帧盘口快照，
// 之后每有成交或盘口变化就实时推送。
//
// 推送是「快照式」的：depth 消息总是最新盘口全量，客户端直接覆盖本地状态即可，
// 不需要自己维护增量，也就不用担心丢帧导致状态漂移。
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade 失败时已经写好了 HTTP 错误响应，这里只记录一笔
		log.Printf("websocket upgrade: %v", err)
		return
	}
	defer conn.Close()

	// 先订阅再发快照：反过来的话，快照写完到订阅生效之间的成交会丢，
	// 客户端可能一直停在旧盘口上，直到下一笔成交才更新。
	msgs, unsubscribe := h.hub.Subscribe()
	defer unsubscribe()

	// 连上先给快照，客户端不用再调一次 REST 才能渲染盘口
	if err := writeWS(conn, wsMessage{Type: "depth", Depth: h.depthSnapshot()}); err != nil {
		return
	}

	// 读循环只做一件事：感知断开（关闭帧或网络错误）。
	// 客户端不需要发任何消息，读到内容直接丢弃。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			// gorilla 的 Conn 不允许并发写，因此写只发生在这个 goroutine 里
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}
}

// depthSnapshot 取当前盘口，转成对外的 DTO 形态。
func (h *Handler) depthSnapshot() *depthResponse {
	bids, asks := h.engine.Depth(wsDepthLevels)
	return &depthResponse{Bids: toLevelDTOs(bids), Asks: toLevelDTOs(asks)}
}

// broadcastTrades 把本次撮合产生的成交逐笔推给订阅者。
func (h *Handler) broadcastTrades(trades []engine.Trade) {
	for _, t := range trades {
		dto := toTradeDTO(t)
		h.hub.Publish(wsMessage{Type: "trade", Trade: &dto})
	}
}

// broadcastDepth 推送一次盘口快照。
func (h *Handler) broadcastDepth() {
	h.hub.Publish(wsMessage{Type: "depth", Depth: h.depthSnapshot()})
}

// writeWS 直接向单个连接写一条消息，仅用于连接建立时的首帧。
func writeWS(conn *websocket.Conn, msg wsMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}
