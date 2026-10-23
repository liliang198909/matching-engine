// Command api 以 HTTP 服务形式对外提供撮合引擎能力。
//
// 与 cmd/demo 的区别：demo 是一次性演示脚本，跑完即退出；
// 本程序是长驻服务，启动后持续等待外部请求。
// 同一个仓库可以有多个可执行入口，各自建立在同一套 engine 之上。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/liliang198909/matching-engine/api"
	"github.com/liliang198909/matching-engine/engine"
	"github.com/liliang198909/matching-engine/store"
)

func main() {
	eng := engine.NewEngine()

	// orders 为 nil 时 api 层退化为纯内存模式。
	// 这样本地只想跑一跑、或者临时演示时，不需要先把数据库搭起来。
	var orders api.OrderStore
	if dsn := os.Getenv("DATABASE_URL"); dsn == "" {
		log.Print("DATABASE_URL is not set: running in memory only, state is lost on restart")
	} else {
		pg := openStore(dsn)
		defer pg.Close()
		orders = pg
		restore(eng, pg)
	}

	addr := listenAddr()

	// 显式构造 http.Server 而不是直接用 http.ListenAndServe：
	// 默认的 http.Server 没有超时，一个只发请求头却不继续的慢速连接
	// 能一直占用 goroutine，属于常见的资源耗尽风险。
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewHandler(eng, orders).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("matching-engine API listening on http://localhost%s", addr)

	// ListenAndServe 正常情况下永不返回；返回即代表出错。
	// http.ErrServerClosed 是主动关闭时的正常信号，不应视为故障。
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped unexpectedly: %v", err)
	}
}

// listenAddr 解析监听地址的优先级：ADDR > PORT > 默认值。
//
// PORT 是 Render、Koyeb、Heroku 这类托管平台的约定：平台在运行时
// 动态注入端口号，服务必须监听它，否则平台的健康检查找不到实例。
// 这是个通用的十二要素应用惯例，加上后本地和云端用同一份代码。
func listenAddr() string {
	if addr := os.Getenv("ADDR"); addr != "" {
		return addr
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

// openStore 连接数据库并建表。连不上直接终止：一个声称持久化、
// 实际却悄悄退回内存的服务，比启动失败更危险。
func openStore(dsn string) *store.Postgres {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pg, err := store.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	if err := pg.Migrate(ctx); err != nil {
		pg.Close()
		log.Fatalf("migrate schema: %v", err)
	}
	return pg
}

// restore 用历史订单回放重建订单簿与成交记录。
//
// 这一步必须在开始接收请求之前完成，否则新订单会和历史订单乱序，
// 撮合结果就不再与崩溃前一致。
func restore(eng *engine.Engine, pg *store.Postgres) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	records, err := pg.LoadOrders(ctx)
	if err != nil {
		log.Fatalf("load order log: %v", err)
	}

	replay := make([]engine.ReplayOrder, 0, len(records))
	for _, r := range records {
		replay = append(replay, engine.ReplayOrder{
			ID:    r.ID,
			Size:  r.Size,
			Price: r.Price,
			Bid:   r.Side == "buy",
		})
	}
	eng.Replay(replay)

	log.Printf("restored %d orders, rebuilt %d trades", len(replay), len(eng.Trades()))
}
