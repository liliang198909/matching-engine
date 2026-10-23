package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/liliang198909/matching-engine/engine"
)

func TestHubBroadcastsToAllSubscribers(t *testing.T) {
	hub := NewHub()
	first, stopFirst := hub.Subscribe()
	defer stopFirst()
	second, stopSecond := hub.Subscribe()
	defer stopSecond()

	hub.Publish(wsMessage{Type: "depth", Depth: &depthResponse{}})

	for name, ch := range map[string]<-chan []byte{"first": first, "second": second} {
		select {
		case data := <-ch:
			var msg wsMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("%s subscriber: decode: %v", name, err)
			}
			if msg.Type != "depth" {
				t.Fatalf("%s subscriber: type = %q, want depth", name, msg.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s subscriber received nothing", name)
		}
	}

	stopFirst()
	if n := hub.clientsCount(); n != 1 {
		t.Fatalf("clients = %d, want 1 after unsubscribe", n)
	}

	// 已注销的订阅者通道会被关闭：能立即读到 ok=false，而不是新消息
	select {
	case _, ok := <-first:
		if ok {
			t.Fatal("cancelled subscriber still received a message")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancelled subscriber channel was not closed")
	}

	// 取消函数必须可以安全地重复调用，否则 handler 里的 defer 会 panic
	stopFirst()
}

func TestWebSocketStreamsTradesAndDepth(t *testing.T) {
	srv := httptest.NewServer(NewHandler(engine.NewEngine(), nil).Routes())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 第一帧应当是当前盘口快照（此时为空）
	if msg := readWSMessage(t, conn); msg.Type != "depth" || len(msg.Depth.Asks) != 0 {
		t.Fatalf("first frame = %+v, want empty depth snapshot", msg)
	}

	post(t, srv.URL+"/orders", `{"side":"sell","price":100,"size":5}`)

	// 挂单不产生成交，只推送一帧新盘口
	msg := readWSMessage(t, conn)
	if msg.Type != "depth" || len(msg.Depth.Asks) != 1 || msg.Depth.Asks[0].Volume != 5 {
		t.Fatalf("after resting order: %+v, want ask 100 x 5", msg)
	}

	post(t, srv.URL+"/orders", `{"side":"buy","price":100,"size":5}`)

	// 吃单先推成交，再推盘口——顺序固定，客户端可以据此更新成交流水
	msg = readWSMessage(t, conn)
	if msg.Type != "trade" || msg.Trade.Price != 100 || msg.Trade.Size != 5 {
		t.Fatalf("after matching order: %+v, want a trade at 100 x 5", msg)
	}

	msg = readWSMessage(t, conn)
	if msg.Type != "depth" || len(msg.Depth.Asks) != 0 {
		t.Fatalf("after fill: %+v, want empty asks", msg)
	}
}

func post(t *testing.T, url, body string) {
	t.Helper()

	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// readWSMessage 读取一帧并解码，超时即判定失败，避免测试永久挂起。
func readWSMessage(t *testing.T, conn *websocket.Conn) wsMessage {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}

	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return msg
}
