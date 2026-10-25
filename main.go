package main

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tvbox-manager/internal/api"
	"tvbox-manager/internal/scheduler"
	"tvbox-manager/internal/store"
)

//go:embed all:web
var webFS embed.FS

const version = "1.0.0"

func main() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	st, err := store.New(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化数据目录失败: %v\n", err)
		os.Exit(1)
	}
	cfg := st.Config()

	sch := scheduler.Start(st, &cfg)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           api.New(st, sch, version, webFS).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		fmt.Printf("==============================================\n")
		fmt.Printf("  tvbox-manager v%s 已启动\n", version)
		fmt.Printf("  管理后台:  http://127.0.0.1:%d/\n", cfg.Port)
		fmt.Printf("  订阅地址:  http://127.0.0.1:%d%s   ← 填入 TVBox 客户端\n", cfg.Port, cfg.SubPath)
		fmt.Printf("  数据目录:  %s\n", dataDir)
		fmt.Printf("==============================================\n")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "HTTP 服务退出: %v\n", err)
			os.Exit(1)
		}
	}()

	// 优雅关闭：等待进行中的探活与请求
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	fmt.Println("收到退出信号，正在优雅关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	sch.Shutdown()
	fmt.Println("已退出")
}
