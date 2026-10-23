// Command api 以 HTTP 服务形式对外提供撮合引擎能力。
//
// 与 cmd/demo 的区别：demo 是一次性演示脚本，跑完即退出；
// 本程序是长驻服务，启动后持续等待外部请求。
// 同一个仓库可以有多个可执行入口，各自建立在同一套 engine 之上。
package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/liliang198909/matching-engine/api"
	"github.com/liliang198909/matching-engine/engine"
)

func main() {
	// 监听地址优先取环境变量，便于部署时由容器或平台注入；
	// 未设置则用本地开发默认值。
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// 显式构造 http.Server 而不是直接用 http.ListenAndServe：
	// 默认的 http.Server 没有超时，一个只发请求头却不继续的慢速连接
	// 能一直占用 goroutine，属于常见的资源耗尽风险。
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewHandler(engine.NewEngine()).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("matching-engine API listening on http://localhost%s", addr)

	// ListenAndServe 正常情况下永不返回；返回即代表出错。
	// http.ErrServerClosed 是主动关闭时的正常信号，不应视为故障。
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped unexpectedly: %v", err)
	}
}
