// Command api 启动制动分配阀整周期验算的纯后端 HTTP 服务。
// 仅使用 Go 1.24 标准库 net/http；监听端口由 PORT 环境变量控制（默认 8080）。
package main

import (
	"log"
	"net/http"
	"os"

	"brakealign"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("brake alignment verification API listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: align.Handler()}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
