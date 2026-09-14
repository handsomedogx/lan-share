// Package logger 提供极简的日志记录。
//
// 路由器环境的两条硬约束：
//  1. 日志量必须可控 —— 绝不记录每个 WebSocket 心跳、每个 HTTP 请求；
//  2. 日志文件禁止无限增长 —— 超过阈值自动轮转，只保留一份历史。
//
// 只记录：服务启动/停止、异常、登录失败、文件上传/删除、WebSocket 重大错误。
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// maxLogSize 是单个日志文件的大小上限（2 MB）。超过即轮转。
const maxLogSize = 2 * 1024 * 1024

// Logger 是一个带大小轮转的简单日志器。
type Logger struct {
	mu   sync.Mutex
	file *os.File
	path string
	std  *log.Logger
}

// New 创建日志器。日志同时写入文件（受大小限制）与 stderr。
//
// stderr 这一路是为了让 procd / logread 能抓到关键信息，即使文件写入失败
// 服务依然可观测。
func New(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}

	l := &Logger{path: path}
	if err := l.open(); err != nil {
		// 日志文件开不了不应该让服务起不来，退化为只写 stderr。
		l.std = log.New(os.Stderr, "[lan-share] ", log.LstdFlags)
		return l, nil
	}

	l.std = log.New(io.MultiWriter(os.Stderr, l), "[lan-share] ", log.LstdFlags)
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.file = f
	return nil
}

// Write 实现 io.Writer，并在写入前检查是否需要轮转。
func (l *Logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return len(p), nil
	}

	if st, err := l.file.Stat(); err == nil && st.Size() > maxLogSize {
		l.rotateLocked()
	}
	return l.file.Write(p)
}

// rotateLocked 把当前日志改名为 .1（覆盖旧的 .1），然后重新打开空文件。
// 调用者必须已持有 l.mu。
func (l *Logger) rotateLocked() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	_ = os.Rename(l.path, l.path+".1")
	_ = l.open()
}

// Info 记录一般信息。
func (l *Logger) Info(format string, args ...any) { l.std.Printf(format, args...) }

// Error 记录异常。
func (l *Logger) Error(format string, args ...any) { l.std.Printf("[ERROR] "+format, args...) }

// Close 关闭日志文件。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
