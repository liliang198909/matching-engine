// Package api 提供撮合引擎的 HTTP 接入层。
//
// 分层原则：engine 包只关心撮合逻辑，完全不感知 HTTP；
// 本包负责协议转换——把 JSON 请求翻译成引擎调用，再把引擎结果翻译回 JSON。
// 依赖方向单向：api → engine，engine 不反过来依赖 api。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/liliang198909/matching-engine/engine"
)

// defaultDepth 是查询盘口时默认返回的档位数。
const defaultDepth = 5

// persistTimeout 限制一次落库的最长耗时，避免数据库卡住时请求无限等待。
const persistTimeout = 5 * time.Second

// OrderStore 是接入层对持久化层的全部要求。
//
// 只声明用到的方法，而不是直接依赖 *store.Postgres：
// 依赖的是能力（能追加一条事件日志），不是某个具体实现。
// 好处是测试里可以塞一个假实现，不必真的连数据库。
type OrderStore interface {
	InsertOrder(ctx context.Context, side string, price, size float64) (uint64, error)
	InsertCancel(ctx context.Context, orderID uint64) error
}

// Handler 持有引擎实例，为每个 HTTP 请求提供入口。
//
// 这里不需要额外加锁：engine.Engine 内部已用互斥锁保护共享状态，
// 多个请求并发调用 SubmitOrder 是安全的。
type Handler struct {
	engine *engine.Engine
	orders OrderStore // 为 nil 时退化为纯内存模式
}

// NewHandler 创建 HTTP 处理器。
// orders 传 nil 表示不落盘，服务重启后订单与成交全部丢失。
func NewHandler(e *engine.Engine, orders OrderStore) *Handler {
	return &Handler{engine: e, orders: orders}
}

// Routes 注册全部路由，返回可直接交给 http.Server 的 http.Handler。
// 返回接口而不是具体类型，是 Go 里"面向接口编程"的常见写法：
// 调用方只关心它能处理 HTTP 请求，不关心底层用的是哪种 mux。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Go 1.22 起 ServeMux 支持「方法 + 路径」模式。
	// 方法不匹配时标准库直接返回 405，省掉了手写 r.Method 判断的样板代码。
	mux.HandleFunc("POST /orders", h.submitOrder)
	mux.HandleFunc("DELETE /orders/{id}", h.cancelOrder)
	mux.HandleFunc("GET /depth", h.getDepth)
	mux.HandleFunc("GET /trades", h.getTrades)
	mux.HandleFunc("GET /healthz", h.health)

	return mux
}

// ---------------------------------------------------------------- 请求/响应结构

// orderRequest 是下单接口的请求体。
//
// 两个要点：
//  1. 字段名首字母必须大写，否则 encoding/json 无法访问（反射只能读导出字段）
//  2. json tag 决定对外暴露的字段名。Go 习惯用驼峰，JSON 习惯用下划线，
//     有了 tag 两者可以各守其道，不必为了接口好看而破坏 Go 命名规范。
type orderRequest struct {
	Side  string  `json:"side"`  // "buy" 或 "sell"
	Price float64 `json:"price"` // 限价，必须大于 0
	Size  float64 `json:"size"`  // 数量，必须大于 0
}

// bid 校验请求并把 side 翻译成引擎需要的 bool。
//
// 把校验写成请求结构体的方法，而不是散落在 handler 里，
// 是为了让 handler 只保留"编解码 + 调用引擎"这一条主线。
func (r orderRequest) bid() (bool, error) {
	if r.Price <= 0 {
		return false, errors.New("price must be greater than 0")
	}
	if r.Size <= 0 {
		return false, errors.New("size must be greater than 0")
	}

	switch r.Side {
	case "buy":
		return true, nil
	case "sell":
		return false, nil
	default:
		return false, errors.New(`side must be "buy" or "sell"`)
	}
}

// tradeDTO 是成交记录的对外表示。
//
// 为什么不直接序列化 engine.Trade？
// 因为领域类型属于引擎内部模型，一旦给它加 json tag，
// 它的字段名就被外部协议绑死了，以后重构引擎会破坏 API 兼容性。
// 多写一个 DTO 换来的是两层可以独立演进。
type tradeDTO struct {
	Price    float64 `json:"price"`
	Size     float64 `json:"size"`
	BidOrder uint64  `json:"bid_order"`
	AskOrder uint64  `json:"ask_order"`
}

// orderResponse 是下单接口的响应体，包含本次产生的全部成交。
// 未成交时会返回空数组而不是 null，客户端可以无脑遍历。
type orderResponse struct {
	Trades []tradeDTO `json:"trades"`
}

// cancelResponse 是撤单成功的响应体。
type cancelResponse struct {
	OrderID   uint64 `json:"order_id"`
	Cancelled bool   `json:"cancelled"`
}

// levelDTO 是盘口单个价格档位的对外表示。
type levelDTO struct {
	Price  float64 `json:"price"`
	Volume float64 `json:"volume"`
}

// depthResponse 是盘口深度响应，买盘按价格降序、卖盘按价格升序。
type depthResponse struct {
	Bids []levelDTO `json:"bids"`
	Asks []levelDTO `json:"asks"`
}

// tradesResponse 是历史成交响应。
type tradesResponse struct {
	Trades []tradeDTO `json:"trades"`
}

// errorResponse 是统一的错误响应体，永远只有一个 error 字段。
type errorResponse struct {
	Error string `json:"error"`
}

// ---------------------------------------------------------------- handler

// submitOrder 处理 POST /orders：提交一笔限价单并返回撮合结果。
//
// 函数签名 (w http.ResponseWriter, r *http.Request) 是标准库规定的
// http.HandlerFunc 形态，写成方法则成为 Handler 的成员，可以访问 h.engine。
func (h *Handler) submitOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	bid, err := req.bid()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	trades, err := h.submit(r.Context(), req, bid)
	if err != nil {
		// 落库失败就必须拒绝请求：如果先撮合后写库，
		// 写库失败会导致内存里的订单簿包含了数据库没有的订单，
		// 重启回放后状态就对不上了。宁可让调用方重试。
		log.Printf("persist order: %v", err)
		writeError(w, http.StatusServiceUnavailable, "could not persist order")
		return
	}

	writeJSON(w, http.StatusCreated, orderResponse{Trades: toTradeDTOs(trades)})
}

// submit 先落库拿到订单 ID，再让引擎按该 ID 撮合。
//
// 为什么不直接调 engine.SubmitOrder 写库？那样订单 ID 由引擎自己数，
// 进程重启后会从 1 重新开始，与历史订单撞号。交给数据库序列分配，
// 编号才能跨重启保持唯一且连续。
func (h *Handler) submit(ctx context.Context, req orderRequest, bid bool) ([]engine.Trade, error) {
	if h.orders == nil {
		return h.engine.SubmitOrder(req.Size, req.Price, bid), nil
	}

	// 与客户端连接解绑：写库一旦提交，即使客户端已断开也应算成功，
	// 否则会出现「数据库有、返回却说失败」的裂缝，重启回放时凭空多出订单。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()

	id, err := h.orders.InsertOrder(ctx, req.Side, req.Price, req.Size)
	if err != nil {
		return nil, err
	}
	return h.engine.SubmitWithID(id, req.Size, req.Price, bid), nil
}

// cancelOrder 处理 DELETE /orders/{id}：撤销一笔仍挂在订单簿上的订单。
//
// {id} 是 Go 1.22 起 ServeMux 支持的路径通配符，取值用 r.PathValue 读取。
func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}

	// 先确认订单真的还挂着，再写撤单日志。
	// 对已成交或不存在的订单，交易所的撤单接口同样不会做任何事。
	if !h.engine.HasOrder(id) {
		writeError(w, http.StatusNotFound, "order not found or already filled")
		return
	}

	if err := h.cancel(r.Context(), id); err != nil {
		// 与下单同理：日志写不进去就绝不改内存状态，
		// 否则重启回放时这笔撤单会凭空消失，订单又活过来。
		log.Printf("persist cancellation: %v", err)
		writeError(w, http.StatusServiceUnavailable, "could not persist cancellation")
		return
	}

	// 前面校验过，但这里仍可能失败：两个撤单请求并发时，
	// 另一个可能已经把订单撤掉了。
	if !h.engine.CancelOrder(id) {
		writeError(w, http.StatusNotFound, "order not found or already filled")
		return
	}

	writeJSON(w, http.StatusOK, cancelResponse{OrderID: id, Cancelled: true})
}

// cancel 把撤单写进事件日志。未配置持久化时直接返回 nil。
func (h *Handler) cancel(ctx context.Context, id uint64) error {
	if h.orders == nil {
		return nil
	}

	ctx, done := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer done()

	return h.orders.InsertCancel(ctx, id)
}

// getDepth 处理 GET /depth?n=5：返回盘口前 n 档。
func (h *Handler) getDepth(w http.ResponseWriter, r *http.Request) {
	n := defaultDepth

	// 查询参数都是字符串，需要显式转成整数并处理失败情况。
	// 这里不做静默兜底：参数写错应当立刻告诉调用方，而不是假装用了默认值。
	if raw := r.URL.Query().Get("n"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "n must be a positive integer")
			return
		}
		n = parsed
	}

	bids, asks := h.engine.Depth(n)
	writeJSON(w, http.StatusOK, depthResponse{
		Bids: toLevelDTOs(bids),
		Asks: toLevelDTOs(asks),
	})
}

// getTrades 处理 GET /trades：返回全部历史成交。
func (h *Handler) getTrades(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, tradesResponse{Trades: toTradeDTOs(h.engine.Trades())})
}

// health 供负载均衡器和容器健康检查使用，只需返回 200。
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------- 辅助函数

// writeJSON 统一写出 JSON 响应：先设响应头，再写状态码，最后写响应体。
//
// 顺序不能颠倒——WriteHeader 一旦调用，响应头就被冻结，
// 之后再 Set 头部的 Content-Type 将不生效。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// 响应已开始写出后无法再改状态码，编码失败也只能放弃，
	// 因此这里显式忽略错误，而不是让错误静默消失。
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError 写出统一格式的错误响应。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// toTradeDTOs 把引擎的成交记录转换为对外结构。
// 永远返回非 nil 切片，保证 JSON 输出是 [] 而不是 null。
func toTradeDTOs(trades []engine.Trade) []tradeDTO {
	out := make([]tradeDTO, 0, len(trades))
	for _, t := range trades {
		out = append(out, tradeDTO{
			Price:    t.Price,
			Size:     t.Size,
			BidOrder: t.BidOrder,
			AskOrder: t.AskOrder,
		})
	}
	return out
}

// toLevelDTOs 把盘口档位转换为对外结构。
func toLevelDTOs(levels []engine.PriceLevel) []levelDTO {
	out := make([]levelDTO, 0, len(levels))
	for _, l := range levels {
		out = append(out, levelDTO{Price: l.Price, Volume: l.Volume})
	}
	return out
}
