// Package config 负责解析运行参数与环境变量。
//
// 设计前提：本程序长期运行在 Kwrt/OpenWrt 路由器上，因此所有可持久化的
// 数据（数据库、文件、日志）都必须落在「大容量数据分区」，不能写 Overlay。
// 因此这里的所有默认路径都指向数据分区，并允许通过环境变量覆盖以便本地开发。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 保存一次运行所需的全部配置。
type Config struct {
	// Addr 是 HTTP 监听地址。
	//
	// 安全设计：默认只监听 127.0.0.1:18080，由 nginx 反向代理对外提供服务，
	// 这样即使路由器有其他对外接口（校园网等）也无法直接访问到 Go 服务端口。
	Addr string

	// Root 是数据根目录，其下包含 data/ files/ logs/。
	Root string

	// DatabasePath 是 SQLite 数据库文件路径。
	DatabasePath string

	// FilesRoot 是文件仓库根目录，其下分 permanent/ 与 chat/。
	FilesRoot string

	// LogPath 是日志文件路径，会做大小限制防止无限增长。
	LogPath string

	// TmpDir 是临时文件目录，会写进进程自身的 TMPDIR 环境变量。
	//
	// 为什么程序要自己管这件事：OpenWrt 的 /tmp 是 tmpfs —— 它占的是内存。
	// 标准库（os.CreateTemp、multipart）默认就往 TMPDIR 落临时文件，
	// 于是「传一个 100MB 的文件」≈「吃掉 100MB 内存」，曾直接触发 OOM Killer。
	// 部署脚本也会设 TMPDIR（见 deploy/*.init），这里只是给「手工跑二进制」
	// 的场景兜底，保证不管怎么启动都写数据分区。
	TmpDir string

	// SessionTTL 是会话 Cookie 的有效期（秒）。
	SessionTTL int

	// MaxUploadBytes 是单次上传的大小上限，0 表示不限制。
	MaxUploadBytes int64

	// ChatMaxUploadBytes 是聊天室单文件上限，0 表示不限制。
	//
	// 与 MaxUploadBytes 分开，是因为两者定位不同：
	// 文件仓库是「长期保存」，可以放开；聊天室是「即时投递」，
	// 前端的进度条与房间生命周期都不适合超大文件。
	ChatMaxUploadBytes int64

	// UploadConcurrency 是服务端同时处理的上传数，<=0 表示不限。
	//
	// 默认 2 是给路由器定的：同时进行多份「收网络 + 写盘 + SHA256 + fsync」
	// 会明显抬高负载，而 1～2 路已经能把千兆局域网的磁盘吃满。
	UploadConcurrency int
}

const (
	// DefaultListen 是默认监听地址，只对本机开放。
	//
	// 默认回环是刻意的保守值：不显式配置就不会意外暴露服务。
	// 部署时可以按需改成 0.0.0.0:18080（直连模式，见 deploy/lan-share-direct.init）
	// 或保持回环走 nginx 反代（见 deploy/lan-share.init）。
	// 非回环监听会在启动日志里打一条 ERROR 级告警（见 cmd/server/main.go），
	// 这是提醒而非报错 —— 两种模式都是受支持的。
	DefaultListen = "127.0.0.1:18080"

	// DefaultRoot 是路由器上的默认数据根目录。
	DefaultRoot = "/mnt/data_mmcblk0p27/lan-share"

	// DefaultUploadConcurrency 是默认的同时上传数。
	//
	// 前端也按这个数排队（web/js/app.js 的 UPLOAD_CONCURRENCY），
	// 两边保持一致，正常使用时就不会撞到服务端排队。
	DefaultUploadConcurrency = 2

	// DefaultChatMaxUpload 是聊天室单文件默认上限（100 MB）。
	//
	// 选 100MB 的理由：局域网内几百兆的安装包也常见，
	// 但聊天室文件随房间消失、不适合当网盘用，所以给一个
	// 「够传文档、图片、小压缩包」的克制值。
	DefaultChatMaxUpload int64 = 100 * 1024 * 1024
)

// Load 从环境变量读取配置，未设置的项使用适合路由器的默认值。
//
// 支持的变量：
//
//	LANSHARE_LISTEN         监听地址，默认 127.0.0.1:18080
//	LANSHARE_ROOT           数据根目录
//	LANSHARE_DB             SQLite 路径
//	LANSHARE_FILES          文件仓库根目录
//	LANSHARE_LOG            日志文件路径
//	LANSHARE_MAX_UPLOAD_MB  文件仓库单次上传上限（MB），0 或留空表示不限制
//	LANSHARE_CHAT_UPLOAD_MB 聊天室单文件上限（MB），0 表示聊天室不限制，
//	                        留空用 DefaultChatMaxUpload（100 MB）
//
// 两个上传上限**完全独立**：各自解析、各自生效，互不回落、互不覆盖。
// 早先的实现会把「未配置」当成「用另一个变量的值」，于是
// 「仓库不限 + 聊天室 100MB」这个最常见的组合根本表达不出来 ——
// 现在 LANSHARE_CHAT_UPLOAD_MB=0 就是聊天室不限，不会再被仓库的值顶掉。
//
//	LANSHARE_UPLOAD_CONCURRENCY 服务端同时处理的上传数，0 表示不限，默认 2
func Load() (*Config, error) {
	root := env("LANSHARE_ROOT", DefaultRoot)

	cfg := &Config{
		Addr:               env("LANSHARE_LISTEN", DefaultListen),
		Root:               root,
		DatabasePath:       env("LANSHARE_DB", filepath.Join(root, "data", "lan-share.db")),
		FilesRoot:          env("LANSHARE_FILES", filepath.Join(root, "files")),
		LogPath:            env("LANSHARE_LOG", filepath.Join(root, "logs", "lan-share.log")),
		TmpDir:             env("TMPDIR", filepath.Join(root, "tmp")),
		SessionTTL:         30 * 24 * 3600,       // 30 天
		ChatMaxUploadBytes: DefaultChatMaxUpload, // 100 MB
		UploadConcurrency:  DefaultUploadConcurrency,
	}

	mb := env("LANSHARE_MAX_UPLOAD_MB", "0")
	if mb != "0" && mb != "" {
		var v int64
		if _, err := fmt.Sscanf(mb, "%d", &v); err != nil {
			return nil, fmt.Errorf("解析 LANSHARE_MAX_UPLOAD_MB 失败: %w", err)
		}
		cfg.MaxUploadBytes = v * 1024 * 1024
	}

	// 上传并发数。设 0 可以关掉服务端限流（只留前端排队），
	// 一般没必要动 —— 这个闸门的意义就在防绕过前端的客户端。
	if cs := env("LANSHARE_UPLOAD_CONCURRENCY", ""); cs != "" {
		var v int
		if _, err := fmt.Sscanf(cs, "%d", &v); err != nil {
			return nil, fmt.Errorf("解析 LANSHARE_UPLOAD_CONCURRENCY 失败: %w", err)
		}
		cfg.UploadConcurrency = v
	}

	// 聊天室上传上限单独一个变量：它和文件仓库的定位不同，
	// 不该被「仓库不限大小」顺手带成无限。
	if cmb := env("LANSHARE_CHAT_UPLOAD_MB", ""); cmb != "" {
		var v int64
		if _, err := fmt.Sscanf(cmb, "%d", &v); err != nil {
			return nil, fmt.Errorf("解析 LANSHARE_CHAT_UPLOAD_MB 失败: %w", err)
		}
		if v <= 0 {
			cfg.ChatMaxUploadBytes = 0 // 显式 0 = 也在聊天室放开限制
		} else {
			cfg.ChatMaxUploadBytes = v * 1024 * 1024
		}
	}

	return cfg, nil
}

// EnsureDirs 创建运行所需的全部目录，权限 0755。
func (c *Config) EnsureDirs() error {
	dirs := []string{
		filepath.Dir(c.DatabasePath),
		filepath.Join(c.FilesRoot, "permanent"),
		filepath.Join(c.FilesRoot, "chat"),
		filepath.Dir(c.LogPath),
		c.TmpDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", d, err)
		}
	}

	// 把 TMPDIR 指回数据分区。必须在任何临时文件产生之前执行 ——
	// os.TempDir() 每次调用都重新读这个环境变量，所以在这里设是有效的。
	//
	// 若外部（procd）已经设过，这里设的是同一个值，无副作用。
	if err := os.Setenv("TMPDIR", c.TmpDir); err != nil {
		return fmt.Errorf("设置 TMPDIR 失败: %w", err)
	}
	return nil
}

// IsLoopback 判断监听地址是否只对本机开放。
// 用于在启动时给出安全警告，避免开发者不小心写成 0.0.0.0。
func (c *Config) IsLoopback() bool {
	host := c.Addr
	if i := strings.LastIndex(c.Addr, ":"); i >= 0 {
		host = c.Addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
