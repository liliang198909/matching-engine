// Command api 以 HTTP 服务形式对外提供撮合引擎能力。
//
// 与 cmd/demo 的区别：demo 是一次性演示脚本，跑完即退出；
// 本程序是长驻服务，启动后持续等待外部请求。
// 同一个仓库可以有多个可执行入口，各自建立在同一套 engine 之上。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/liliang198909/matching-engine/api"
	"github.com/liliang198909/matching-engine/chain"
	"github.com/liliang198909/matching-engine/engine"
	"github.com/liliang198909/matching-engine/store"
)

func main() {
	eng := engine.NewEngine()

	// orders 为 nil 时 api 层退化为纯内存模式。
	// 这样本地只想跑一跑、或者临时演示时，不需要先把数据库搭起来。
	var (
		orders api.OrderStore
		pg     *store.Postgres // 链上通道需要它；纯内存模式下为 nil
	)
	if dsn := os.Getenv("DATABASE_URL"); dsn == "" {
		log.Print("DATABASE_URL is not set: running in memory only, state is lost on restart")
	} else {
		pg = openStore(dsn)
		defer pg.Close()
		orders = pg
		restore(eng, pg)
	}

	handler := api.NewHandler(eng, orders)
	if pg != nil {
		handler.AttachTransfers(pg)
	}

	// 链上监听与 HTTP 服务共享同一个 ctx：进程退出时一起收敛。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cleanup, started, err := startChainWatcher(ctx, pg)
	if err != nil {
		log.Fatalf("start chain watcher: %v", err)
	}
	if started {
		defer cleanup()
	}

	addr := listenAddr()

	// 显式构造 http.Server 而不是直接用 http.ListenAndServe：
	// 默认的 http.Server 没有超时，一个只发请求头却不继续的慢速连接
	// 能一直占用 goroutine，属于常见的资源耗尽风险。
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler.Routes(),
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

// startChainWatcher 按环境变量装配链上 Transfer 监听。
//
// 两个环境变量缺一不可：
//   - CHAIN_RPC_URL   节点地址（https 即可，watcher 走轮询不依赖 wss）
//   - ERC20_CONTRACT  要监听的合约地址
//
// 没有配置时返回 started=false，服务照常以纯撮合模式运行。
// 配了但写错（地址非法、节点连不上）则直接终止启动——配置错误属于部署期问题，
// 让它悄悄不生效比启动失败更难排查。
func startChainWatcher(ctx context.Context, pg *store.Postgres) (cleanup func(), started bool, err error) {
	rpcURL := os.Getenv("CHAIN_RPC_URL")
	contractHex := os.Getenv("ERC20_CONTRACT")

	if rpcURL == "" || contractHex == "" {
		log.Print("CHAIN_RPC_URL / ERC20_CONTRACT not set: chain watcher disabled")
		return nil, false, nil
	}
	if pg == nil {
		// 没有数据库就没有游标和落库位置，链上通道无从谈起。
		log.Print("chain watcher needs DATABASE_URL: disabled")
		return nil, false, nil
	}

	token, err := chain.ParseToken(contractHex)
	if err != nil {
		return nil, false, err
	}

	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, false, fmt.Errorf("dial chain node: %w", err)
	}

	w := chain.NewWatcher(chain.Config{
		Source: chain.NewRPCSource(client, token),
		Sink:   pg,
	})
	go w.Run(ctx)

	log.Printf("chain watcher following %s", token.Hex())

	// 返回 client.Close 而不是在这里 defer：
	// 函数返回后 watcher 还要继续用这条连接，提前关闭会让它一直报错重试。
	return client.Close, true, nil
}

// restore 用历史事件回放重建订单簿与成交记录。
//
// 这一步必须在开始接收请求之前完成，否则新订单会和历史事件乱序，
// 撮合结果就不再与崩溃前一致。
func restore(eng *engine.Engine, pg *store.Postgres) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	records, err := pg.LoadEvents(ctx)
	if err != nil {
		log.Fatalf("load event log: %v", err)
	}

	events := make([]engine.ReplayEvent, 0, len(records))
	for _, r := range records {
		events = append(events, engine.ReplayEvent{
			OrderID: r.OrderID,
			Size:    r.Size,
			Price:   r.Price,
			Bid:     r.Side == "buy",
			Cancel:  r.Kind == "cancel",
		})
	}
	eng.Replay(events)

	log.Printf("restored %d events, rebuilt %d trades", len(events), len(eng.Trades()))
}
