package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tvbox-manager/internal/api"
	"tvbox-manager/internal/scheduler"
	"tvbox-manager/internal/store"
)

//go:embed all:web
var webFS embed.FS

const version = "1.4.9"

func main() {
	// 开发模式：-web ./web 或 WEB_DIR=./web，前端从磁盘读取，改完刷新浏览器即可，无需重新编译
	webDir := ""
	flag.StringVar(&webDir, "web", "", "开发模式：从磁盘目录读取前端资源（如 ./web），免重新编译")
	flag.Parse()
	if webDir == "" {
		webDir = os.Getenv("WEB_DIR")
	}

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

	// 静态资源根：开发模式取磁盘，生产取内嵌
	webRoot, webFrom, err := resolveWeb(webDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	apiSrv := api.New(st, sch, version, webRoot)
	apiSrv.PastedDir = filepath.Join(dataDir, "pasted")             // 粘贴单仓的本地托管目录
	apiSrv.LiveCacheFile = filepath.Join(dataDir, "live_cache.json") // 直播频道表落盘缓存（重启秒开）
	if webDir != "" {
		apiSrv.DevMode(true)
	}

	srv := &http.Server{
		Handler:           apiSrv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 先绑定端口再打印，端口占用/无权限时能明确报错，避免「显示已启动但页面打不开」
	addr := fmt.Sprintf(":%d", cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "监听 %s 失败（端口可能已被占用）: %v\n", addr, err)
		os.Exit(1)
	}

	cfgFile := filepath.Join(dataDir, "config.json")
	fmt.Printf("==============================================\n")
	fmt.Printf("  tvbox-manager v%s 已启动\n", version)
	fmt.Printf("  管理后台:  http://127.0.0.1:%d/\n", cfg.Port)
	fmt.Printf("  订阅地址:  http://127.0.0.1:%d%s   ← 填入 TVBox 客户端\n", cfg.Port, cfg.SubPath)
	if cfg.Token == "" {
		fmt.Printf("  访问令牌:  （未设置）\n")
		fmt.Printf("  ⚠️  当前后台无鉴权（/api/* 不校验令牌），登录后请在「设置 → 访问令牌」中设置\n")
	} else {
		fmt.Printf("  访问令牌:  %s   ← 登录后台用\n", cfg.Token)
	}
	fmt.Printf("  数据目录:  %s\n", dataDir)
	fmt.Printf("  前端来源:  %s\n", webFrom)
	if cfg.Token != "" {
		fmt.Printf("  （令牌来自 %s，可在后台「设置」中修改；公网部署请注意保护）\n", cfgFile)
	}
	if webDir != "" {
		fmt.Printf("  开发模式:  已开启，改 web/ 下文件后刷新浏览器生效（无需重新编译）\n")
	}
	fmt.Printf("==============================================\n")

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "HTTP 服务退出: %v\n", err)
			os.Exit(1)
		}
	}()

	// 后台预热直播频道表：启动即开始拉取并落盘，用户点「直播」时直接命中缓存秒开，
	// 不必等每次进页面现拉几十个上游 m3u。
	go apiSrv.PreloadLiveChannels()

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

// resolveWeb 返回「以 web 目录为根」的静态资源 FS 及来源描述。
// 传了 dir 走磁盘（开发模式，改完刷新即生效），否则用 go:embed 内嵌资源。
func resolveWeb(dir string) (fs.FS, string, error) {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, "", fmt.Errorf("解析前端目录失败: %w", err)
		}
		if _, err := os.Stat(filepath.Join(abs, "index.html")); err != nil {
			return nil, "", fmt.Errorf("前端目录无效（缺少 index.html）: %s", abs)
		}
		return os.DirFS(abs), "磁盘 " + abs, nil
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, "", fmt.Errorf("内嵌前端资源缺失: %w", err)
	}
	return sub, "内嵌（go:embed）", nil
}
