package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liliang198909/matching-engine/engine"
)

// do 发起一次请求并返回响应与已解码的响应体。
//
// 使用 httptest 而不是真正监听端口：
// 测试直接调用 Handler，不经过网络栈，因此更快也更稳定。
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSubmitOrderRestsOnEmptyBook(t *testing.T) {
	h := NewHandler(engine.NewEngine(), nil).Routes()

	rec := do(t, h, http.MethodPost, "/orders", `{"side":"sell","price":100,"size":5}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}

	var resp orderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// 订单簿为空，卖单不可能成交，只会挂单
	if len(resp.Trades) != 0 {
		t.Fatalf("trades = %d, want 0", len(resp.Trades))
	}
}

func TestBuyOrderMatchesRestingSell(t *testing.T) {
	h := NewHandler(engine.NewEngine(), nil).Routes()

	do(t, h, http.MethodPost, "/orders", `{"side":"sell","price":100,"size":5}`)
	rec := do(t, h, http.MethodPost, "/orders", `{"side":"buy","price":100,"size":5}`)

	var resp orderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(resp.Trades))
	}
	if resp.Trades[0].Price != 100 || resp.Trades[0].Size != 5 {
		t.Fatalf("trade = %+v, want price 100 size 5", resp.Trades[0])
	}
	// 成交后盘口应当被吃空
	depthRec := do(t, h, http.MethodGet, "/depth", "")
	var depth depthResponse
	if err := json.Unmarshal(depthRec.Body.Bytes(), &depth); err != nil {
		t.Fatalf("decode depth: %v", err)
	}
	if len(depth.Bids) != 0 || len(depth.Asks) != 0 {
		t.Fatalf("depth not empty: bids=%v asks=%v", depth.Bids, depth.Asks)
	}
}

func TestSubmitOrderRejectsInvalidInput(t *testing.T) {
	h := NewHandler(engine.NewEngine(), nil).Routes()

	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{"side":`},
		{"unknown side", `{"side":"hold","price":100,"size":1}`},
		{"zero price", `{"side":"buy","price":0,"size":1}`},
		{"negative size", `{"side":"buy","price":100,"size":-1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/orders", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestGetDepthRejectsInvalidN(t *testing.T) {
	h := NewHandler(engine.NewEngine(), nil).Routes()

	for _, path := range []string{"/depth?n=abc", "/depth?n=0", "/depth?n=-3"} {
		rec := do(t, h, http.MethodGet, path, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want %d", path, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := NewHandler(engine.NewEngine(), nil).Routes()

	// 路由声明了 POST /orders，用 GET 访问应由 ServeMux 返回 405
	rec := do(t, h, http.MethodGet, "/orders", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// fakeStore 用内存变量冒充数据库。
//
// 接入层只依赖 OrderStore 这一条方法，所以不必真连 Postgres
// 也能验证「先拿 ID、再撮合」这个顺序是否被遵守。
type fakeStore struct {
	nextID    uint64
	inserted  int
	cancels   []uint64
	err       error
	cancelErr error
}

func (f *fakeStore) InsertOrder(_ context.Context, _ string, _, _ float64) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.nextID++
	f.inserted++
	return f.nextID, nil
}

func (f *fakeStore) InsertCancel(_ context.Context, orderID uint64) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancels = append(f.cancels, orderID)
	return nil
}

func TestSubmitOrderUsesStoreAssignedID(t *testing.T) {
	store := &fakeStore{nextID: 41} // 模拟数据库序列已经走到 41
	h := NewHandler(engine.NewEngine(), store).Routes()

	do(t, h, http.MethodPost, "/orders", `{"side":"sell","price":100,"size":5}`)
	rec := do(t, h, http.MethodPost, "/orders", `{"side":"buy","price":100,"size":5}`)

	var resp orderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(resp.Trades))
	}
	// 两笔订单的 ID 都应来自 store（42、43），而不是引擎内部的 1、2
	if resp.Trades[0].AskOrder != 42 || resp.Trades[0].BidOrder != 43 {
		t.Fatalf("trade order ids = ask %d bid %d, want 42 and 43",
			resp.Trades[0].AskOrder, resp.Trades[0].BidOrder)
	}
	if store.inserted != 2 {
		t.Fatalf("store insertions = %d, want 2", store.inserted)
	}
}

func TestSubmitOrderRejectsWhenStoreFails(t *testing.T) {
	store := &fakeStore{err: errors.New("connection refused")}
	h := NewHandler(engine.NewEngine(), store).Routes()

	rec := do(t, h, http.MethodPost, "/orders", `{"side":"sell","price":100,"size":5}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	// 落库失败时引擎不能被改动：否则内存里会多出一笔数据库没有的挂单
	depthRec := do(t, h, http.MethodGet, "/depth", "")
	var depth depthResponse
	if err := json.Unmarshal(depthRec.Body.Bytes(), &depth); err != nil {
		t.Fatalf("decode depth: %v", err)
	}
	if len(depth.Bids) != 0 || len(depth.Asks) != 0 {
		t.Fatalf("engine mutated after failed persist: bids=%v asks=%v", depth.Bids, depth.Asks)
	}
}

func TestCancelOrder(t *testing.T) {
	store := &fakeStore{nextID: 41} // 下一笔订单拿到 ID 42
	h := NewHandler(engine.NewEngine(), store).Routes()

	do(t, h, http.MethodPost, "/orders", `{"side":"sell","price":100,"size":5}`)

	rec := do(t, h, http.MethodDelete, "/orders/42", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp cancelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OrderID != 42 || !resp.Cancelled {
		t.Fatalf("response = %+v, want order 42 cancelled", resp)
	}

	// 撤单日志必须写进事件表，否则重启回放时这笔订单会复活
	if len(store.cancels) != 1 || store.cancels[0] != 42 {
		t.Fatalf("persisted cancels = %v, want [42]", store.cancels)
	}

	// 唯一一笔挂单被撤，盘口应当清空
	depthRec := do(t, h, http.MethodGet, "/depth", "")
	var depth depthResponse
	if err := json.Unmarshal(depthRec.Body.Bytes(), &depth); err != nil {
		t.Fatalf("decode depth: %v", err)
	}
	if len(depth.Bids) != 0 || len(depth.Asks) != 0 {
		t.Fatalf("depth not empty after cancel: bids=%v asks=%v", depth.Bids, depth.Asks)
	}
}

func TestCancelUnknownOrder(t *testing.T) {
	h := NewHandler(engine.NewEngine(), nil).Routes()

	for _, id := range []string{"1", "999"} {
		rec := do(t, h, http.MethodDelete, "/orders/"+id, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id %s: status = %d, want %d", id, rec.Code, http.StatusNotFound)
		}
	}
}

func TestCancelRejectsInvalidID(t *testing.T) {
	h := NewHandler(engine.NewEngine(), nil).Routes()

	for _, id := range []string{"abc", "0", "-1"} {
		rec := do(t, h, http.MethodDelete, "/orders/"+id, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("id %s: status = %d, want %d", id, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestCancelKeepsOrderWhenStoreFails(t *testing.T) {
	store := &fakeStore{nextID: 41}
	h := NewHandler(engine.NewEngine(), store).Routes()

	do(t, h, http.MethodPost, "/orders", `{"side":"sell","price":100,"size":5}`)
	store.cancelErr = errors.New("connection refused")

	rec := do(t, h, http.MethodDelete, "/orders/42", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	// 日志没写成，内存里的挂单就不能消失
	depthRec := do(t, h, http.MethodGet, "/depth", "")
	var depth depthResponse
	if err := json.Unmarshal(depthRec.Body.Bytes(), &depth); err != nil {
		t.Fatalf("decode depth: %v", err)
	}
	if len(depth.Asks) != 1 || depth.Asks[0].Price != 100 {
		t.Fatalf("order vanished after failed cancel: asks=%v", depth.Asks)
	}
}
