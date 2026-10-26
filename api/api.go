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

	"github.com/liliang198909/matching-engine/chain"
	"github.com/liliang198909/matching-engine/engine"
)

// defaultDepth 是查询盘口时默认返回的档位数。
const defaultDepth = 5

// 成交历史的分页参数约束。
//
// 不设上限的话，一次 /trades 请求就能把整个成交历史复制一遍，
// 历史越长响应越慢、内存峰值越高，本质上是给客户端留了一个放大攻击面。
const (
	defaultTradeLimit = 100
	maxTradeLimit     = 1000
)

// 链上转账同样需要分页上限：一个热门 ERC20 合约一天可能有几十万条 Transfer，
// 不设上限就是一个天然的放大攻击面。
const (
	defaultTransferLimit = 100
	maxTransferLimit     = 1000
)

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

// TransferStore 是链上转账的只读查询能力。
//
// 与 OrderStore 分开声明，而不是合成一个大接口：
// 调用方只依赖自己用到的方法，将来撮合走内存、链上走数据库这种混合部署
// 也能各自满足所需的部分（Go 的接口是隐式实现的，拆分不产生额外成本）。
type TransferStore interface {
	TransferPage(ctx context.Context, offset, limit int) ([]chain.Transfer, int, error)
}

// Handler 持有引擎实例，为每个 HTTP 请求提供入口。
//
// 这里不需要额外加锁：engine.Engine 内部已用互斥锁保护共享状态，
// 多个请求并发调用 SubmitOrder 是安全的。
type Handler struct {
	engine    *engine.Engine
	orders    OrderStore    // 为 nil 时退化为纯内存模式
	transfers TransferStore // 为 nil 时 /transfers 返回 503
	hub       *Hub          // WebSocket 订阅者广播中心
}

// NewHandler 创建 HTTP 处理器。
// orders 传 nil 表示不落盘，服务重启后订单与成交全部丢失。
func NewHandler(e *engine.Engine, orders OrderStore) *Handler {
	return &Handler{engine: e, orders: orders, hub: NewHub()}
}

// AttachTransfers 可选地挂接链上转账查询能力。
//
// 用装配方法而不是多加一个构造参数：现有 18 处调用点无需改动，
// 没有链接上模块的部署形态（纯撮合）保持原样即可。
// 返回自身，便于链式写法 NewHandler(...).AttachTransfers(pg).Routes()。
func (h *Handler) AttachTransfers(t TransferStore) *Handler {
	h.transfers = t
	return h
}

// Routes 注册全部路由，返回可直接交给 http.Server 的 http.Handler。
// 返回接口而不是具体类型，是 Go 里"面向接口编程"的常见写法：
// 调用方只关心它能处理 HTTP 请求，不关心底层用的是哪种 mux。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Go 1.22 起 ServeMux 支持「方法 + 路径」模式。
	// 方法不匹配时标准库直接返回 405，省掉了手写 r.Method 判断的样板代码。
	// "GET /{$}" 只匹配根路径本身；写成 "GET /" 会变成兜底路由，
	// 把所有未知路径都吞掉，真正的 404 就再也看不到了。
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("POST /orders", h.submitOrder)
	mux.HandleFunc("GET /orders/{id}", h.getOrder)
	mux.HandleFunc("DELETE /orders/{id}", h.cancelOrder)
	mux.HandleFunc("GET /depth", h.getDepth)
	mux.HandleFunc("GET /trades", h.getTrades)
	mux.HandleFunc("GET /transfers", h.getTransfers)
	mux.HandleFunc("GET /ws", h.stream)
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

// orderStatusDTO 是查单接口的响应体。
//
// 同样不直接序列化 engine.OrderStatus：领域类型的字段名一旦绑上 json tag，
// 引擎重构就会变成接口变更。
type orderStatusDTO struct {
	OrderID   uint64  `json:"order_id"`
	Side      string  `json:"side"`
	Price     float64 `json:"price"`
	Size      float64 `json:"size"`      // 下单数量
	Filled    float64 `json:"filled"`    // 已成交数量
	Remaining float64 `json:"remaining"` // 剩余数量
	State     string  `json:"state"`     // open / filled / cancelled
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

// tradesResponse 是历史成交的分页响应。
//
// 一并返回 total / limit / offset，客户端不必再猜边界：
// 用 total 就能算出总页数，也不会把「最后一页不满」误判成请求出错。
type tradesResponse struct {
	Trades []tradeDTO `json:"trades"`
	Total  int        `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

// transferDTO 是一条链上 ERC20 转账的对外表示。
//
// value 是十进制字符串而不是数字：链上金额是 256 位整数，
// JSON 的 number 在多数语言里落到 float64，超过 2^53 就会悄悄丢精度。
// 客户端拿到字符串后按需要转成 big.Int 或 decimal 处理。
type transferDTO struct {
	Token       string `json:"token"`
	BlockNumber uint64 `json:"block_number"`
	TxHash      string `json:"tx_hash"`
	LogIndex    uint   `json:"log_index"`
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
}

// transfersResponse 是链上转账的分页响应。
type transfersResponse struct {
	Transfers []transferDTO `json:"transfers"`
	Total     int           `json:"total"`
	Limit     int           `json:"limit"`
	Offset    int           `json:"offset"`
}

// endpointDTO 描述一个可调用的端点。
type endpointDTO struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

// indexResponse 是根路径的服务索引。
//
// 加这个接口纯粹为了可用性：把公网地址发给别人时，对方最自然的动作是
// 直接在浏览器打开根路径，没有索引就会看到一句 404。
type indexResponse struct {
	Service   string        `json:"service"`
	Endpoints []endpointDTO `json:"endpoints"`
}

// errorResponse 是统一的错误响应体，永远只有一个 error 字段。
type errorResponse struct {
	Error string `json:"error"`
}

// wsMessage 是 WebSocket 推送的统一信封，用 type 字段区分两种消息：
// "depth" 携带完整盘口快照，"trade" 携带单笔成交。
type wsMessage struct {
	Type  string         `json:"type"`
	Depth *depthResponse `json:"depth,omitempty"`
	Trade *tradeDTO      `json:"trade,omitempty"`
}

// ---------------------------------------------------------------- handler

// index 处理 GET /：返回服务名与全部端点，方便直接打开链接时看到用法。
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, indexResponse{
		Service: "matching-engine",
		Endpoints: []endpointDTO{
			{Method: "POST", Path: "/orders", Purpose: "Submit a limit order; returns the trades it generated"},
			{Method: "GET", Path: "/orders/{id}", Purpose: "Order status: open / filled / cancelled"},
			{Method: "DELETE", Path: "/orders/{id}", Purpose: "Cancel a resting order"},
			{Method: "GET", Path: "/depth?n=5", Purpose: "Top-n market depth for both sides"},
			{Method: "GET", Path: "/trades?limit=&offset=", Purpose: "Paginated trade history, oldest first"},
			{Method: "GET", Path: "/transfers?limit=&offset=", Purpose: "ERC20 transfers observed on chain (requires chain watcher)"},
			{Method: "GET", Path: "/ws", Purpose: "WebSocket stream: depth snapshots and live trades"},
			{Method: "GET", Path: "/healthz", Purpose: "Liveness probe"},
		},
	})
}

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

	h.broadcastTrades(trades)
	h.broadcastDepth()

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

// getOrder 处理 GET /orders/{id}：查询一笔订单的生命周期状态。
//
// 覆盖已成交与已撤销的订单：它们虽已离开订单簿，历史状态仍需可查，
// 前端轮询下单结果时正需要这个能力。
func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}

	status, ok := h.engine.Status(id)
	if !ok {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}

	side := "sell"
	if status.Bid {
		side = "buy"
	}

	writeJSON(w, http.StatusOK, orderStatusDTO{
		OrderID:   status.ID,
		Side:      side,
		Price:     status.Price,
		Size:      status.Size,
		Filled:    status.Filled,
		Remaining: status.Remaining,
		State:     string(status.State),
	})
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

	h.broadcastDepth()

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

// getTrades 处理 GET /trades?limit=&offset=：分页返回历史成交。
//
// 成交按产生顺序存放，offset 从最早的一笔算起。
// 两个参数都可省略；越界不报错，返回空数组即可。
func (h *Handler) getTrades(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", defaultTradeLimit)
	if err != nil || limit <= 0 || limit > maxTradeLimit {
		writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 1000")
		return
	}

	offset, err := intParam(r, "offset", 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}

	trades, total := h.engine.TradePage(offset, limit)
	writeJSON(w, http.StatusOK, tradesResponse{
		Trades: toTradeDTOs(trades),
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// getTransfers 处理 GET /transfers?limit=&offset=：分页返回链上 ERC20 转账。
//
// 未启用链上模块（没有装 watcher）时返回 503 而不是空列表：
// 空列表会被误读成「链上没有转账」，503 才表达「这个能力没开」。
func (h *Handler) getTransfers(w http.ResponseWriter, r *http.Request) {
	if h.transfers == nil {
		writeError(w, http.StatusServiceUnavailable, "chain watcher is not enabled")
		return
	}

	limit, err := intParam(r, "limit", defaultTransferLimit)
	if err != nil || limit <= 0 || limit > maxTransferLimit {
		writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 1000")
		return
	}

	offset, err := intParam(r, "offset", 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}

	transfers, total, err := h.transfers.TransferPage(r.Context(), offset, limit)
	if err != nil {
		log.Printf("query chain transfers: %v", err)
		writeError(w, http.StatusServiceUnavailable, "could not read chain transfers")
		return
	}

	writeJSON(w, http.StatusOK, transfersResponse{
		Transfers: toTransferDTOs(transfers),
		Total:     total,
		Limit:     limit,
		Offset:    offset,
	})
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
	enc := json.NewEncoder(w)
	// 默认会把 &、<、> 转义成 \u0026 之类的形式（为嵌入 HTML 安全考虑）。
	// 这是纯 JSON 接口，不嵌进 HTML，关掉转义后 URL 之类的字符串才保持可读。
	enc.SetEscapeHTML(false)
	// 响应已开始写出后无法再改状态码，编码失败也只能放弃，
	// 因此这里显式忽略错误，而不是让错误静默消失。
	_ = enc.Encode(payload)
}

// writeError 写出统一格式的错误响应。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// intParam 解析整数查询参数；参数缺失时返回默认值。
//
// 只负责"能不能转成整数"，取值范围由调用方按各自的语义判断：
// limit 的下界是 1，offset 的下界是 0，两者规则不同，不适合塞进同一个函数。
func intParam(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	return strconv.Atoi(raw)
}

// toTradeDTOs 把引擎的成交记录转换为对外结构。
// 永远返回非 nil 切片，保证 JSON 输出是 [] 而不是 null。
func toTradeDTOs(trades []engine.Trade) []tradeDTO {
	out := make([]tradeDTO, 0, len(trades))
	for _, t := range trades {
		out = append(out, toTradeDTO(t))
	}
	return out
}

// toTradeDTO 转换单笔成交，供批量转换与 WebSocket 单笔推送共用。
func toTradeDTO(t engine.Trade) tradeDTO {
	return tradeDTO{
		Price:    t.Price,
		Size:     t.Size,
		BidOrder: t.BidOrder,
		AskOrder: t.AskOrder,
	}
}

// toTransferDTOs 把链上转账转换为对外结构。
// 永远返回非 nil 切片，保证 JSON 输出是 [] 而不是 null。
func toTransferDTOs(transfers []chain.Transfer) []transferDTO {
	out := make([]transferDTO, 0, len(transfers))
	for _, t := range transfers {
		out = append(out, transferDTO{
			Token:       t.Token,
			BlockNumber: t.BlockNumber,
			TxHash:      t.TxHash,
			LogIndex:    t.LogIndex,
			From:        t.From,
			To:          t.To,
			Value:       t.Value,
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
