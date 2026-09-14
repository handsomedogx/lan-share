// Command server 是 LAN Share 的唯一入口。
//
// LAN Share 是一个跑在 Kwrt/OpenWrt 路由器上的局域网内容传输工具：
// 左边是 WebSocket 实时会话，用来随手发文本和链接；
// 右边是需要登录的持久文件仓库。
//
// 编译目标：
//
//	$env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
//	go build -trimpath -ldflags "-s -w" -o lan-share ./cmd/server
//
// 产物是一个独立的 Linux ARM64 ELF，前端资源已用 go:embed 打进二进制，
// 升级时只需要替换这一个文件。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lanshare/internal/api"
	"lanshare/internal/auth"
	"lanshare/internal/cleanup"
	"lanshare/internal/config"
	"lanshare/internal/files"
	"lanshare/internal/httpx"
	"lanshare/internal/logger"
	"lanshare/internal/session"
	"lanshare/internal/storage"
)

// webFS 把前端资源编译进二进制。
//
// 用 embed 而不是外部目录，是文档第 5 节的要求：
// HTML/CSS/JS/Go 全部打成一个文件，升级时只替换一个二进制。
//
//go:embed all:web
var webFS embed.FS

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lan-share 启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		flagInitUser = flag.String("init-user", "", "创建用户，格式 用户名:密码（用户名已存在则跳过）")
		flagShowVer  = flag.Bool("version", false, "打印版本号后退出")
		flagShowPath = flag.Bool("print-paths", false, "打印本次运行实际使用的路径后退出")
	)
	flag.Parse()

	if *flagShowVer {
		fmt.Println("lan-share", api.Version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// -print-paths 必须先于 EnsureDirs：只读地告诉运维「你这条命令会碰哪些文件」，
	// 避免误以为改的是 A 目录、实际改的是 B 目录。
	if *flagShowPath {
		fmt.Println("监听地址:   ", cfg.Addr)
		fmt.Println("数据根目录: ", cfg.Root)
		fmt.Println("数据库:     ", cfg.DatabasePath)
		fmt.Println("文件仓库:   ", cfg.FilesRoot)
		fmt.Println("日志:       ", cfg.LogPath)
		return nil
	}

	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	log, err := logger.New(cfg.LogPath)
	if err != nil {
		return err
	}
	defer log.Close()

	store, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()

	authSvc := auth.New(store)

	// 允许用命令行初始化第一个用户：初始化路由器时没有注册接口可用，
	// 这是唯一的建号途径。
	if *flagInitUser != "" {
		parts := strings.SplitN(*flagInitUser, ":", 2)
		if len(parts) != 2 {
			return errors.New("格式应为 -init-user=用户名:密码")
		}
		// 必须把目标数据库打出来。之前踩过坑：以为在改本地测试库，
		// 实际写进了默认的 /mnt/... 路径，登录时才发现用户根本不在。
		fmt.Println("目标数据库:", cfg.DatabasePath)
		if _, err := store.UserByName(parts[0]); err == nil {
			fmt.Println("用户已存在，未做修改:", parts[0])
			return nil
		}
		// 第一个用户自动成为管理员；之后建的默认是普通用户。
		n, err := store.CountUsers()
		if err != nil {
			return err
		}
		role := storage.RoleUser
		if n == 0 {
			role = storage.RoleAdmin
		}
		hash, err := auth.Hash(parts[1])
		if err != nil {
			return fmt.Errorf("生成密码哈希失败: %w", err)
		}
		u, err := store.CreateUser(parts[0], hash, role)
		if err != nil {
			return fmt.Errorf("创建用户失败: %w", err)
		}
		fmt.Printf("用户创建成功: %s（角色 %s）\n", u.Username, u.Role)
		return nil
	}

	fileSvc, err := files.New(cfg.FilesRoot, cfg.MaxUploadBytes)
	if err != nil {
		return err
	}

	sessions := session.NewManager()
	// 把会话管理器交给清理协程：它需要活跃房间号来识别无主的聊天文件。
	// tmp 目录交给它回收历史临时文件（TMPDIR 已在 config 里指向数据分区）。
	cleaner := cleanup.New(store, fileSvc, sessions, log)
	cleaner.SetTmpDir(cfg.TmpDir)

	// 组装静态资源 handler（去掉 web/ 前缀，让 / 直接对应 web/index.html）。
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("加载内嵌前端资源失败: %w", err)
	}
	static := newStaticHandler(webRoot)

	srv := api.NewServer(api.Options{
		Store:         store,
		Auth:          authSvc,
		Files:         fileSvc,
		Sessions:      sessions,
		Logger:        log,
		WebFS:         static,
		SessionTTL:    cfg.SessionTTL,
		MaxUpload:     cfg.MaxUploadBytes,
		TempFileTTL:   cfg.TempFileDefaultTTL,
		ChatMaxUpload: cfg.ChatMaxUploadBytes,

		UploadConcurrency: cfg.UploadConcurrency,
	})

	httpSrv := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Routes(),
		// 时间参数都放宽：LAN 内传输大文件时慢客户端很常见，超时太短会误杀。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 注意：不设 WriteTimeout —— 下载大文件需要长时间写响应，
		// 设了会导致大文件传到一半被切断。
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go cleaner.Run(ctx)

	log.Info("LAN Share %s 启动", api.Version)
	log.Info("数据根目录: %s", cfg.Root)
	log.Info("数据库: %s", cfg.DatabasePath)
	log.Info("文件仓库: %s", cfg.FilesRoot)
	log.Info("监听地址: %s", cfg.Addr)
	if !cfg.IsLoopback() {
		// 这不是错误 —— 直连模式（0.0.0.0）是受支持的部署方式，
		// 见 deploy/lan-share-direct.init。但暴露面确实变大了，必须显眼提醒：
		// 整个局域网都能访问这个端口。用 ERROR 级是为了让它出现在
		// 任何日志级别的默认视图里，不容易被忽略。
		log.Error("警告：当前监听地址不是回环地址，服务可能直接暴露到局域网/校园网，请确认这符合预期")
	}

	// 启动时顺带清理一次过期会话，避免长期运行后 sessions 表膨胀。
	if n, err := store.CleanupSessions(); err == nil && n > 0 {
		log.Info("启动清理: 移除过期登录会话 %d 条", n)
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Error("HTTP 服务异常退出: %v", err)
		return err
	case <-ctx.Done():
		log.Info("收到退出信号，正在停止服务…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("优雅关闭超时: %v", err)
	}
	log.Info("LAN Share 已停止")
	return nil
}

// newStaticHandler 返回托管内嵌前端资源的 handler。
//
// 两个细节：
//  1. 根路径 "/" 映射到 index.html；
//  2. 静态资源不加长缓存 —— 路由器场景下升级频繁，
//     缓存住旧 JS 导致的「改了没生效」比省那点流量麻烦得多。
func newStaticHandler(root fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}

		if _, err := fs.Stat(root, p); err != nil {
			// 单页应用式回退：未知路径交给 index.html，由前端决定展示什么。
			// 但 API 与 WebSocket 路径不应该走到这里，若发生了就如实返回 404，
			// 避免前端拿到一坨 HTML 却以为是 JSON。
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
				httpx.Fail(w, http.StatusNotFound, "接口不存在")
				return
			}
			serveIndex(w, r, root)
			return
		}

		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	b, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.Error(w, "index.html 未找到", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
