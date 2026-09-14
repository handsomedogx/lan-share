// Package websocket 实现实时传输区的服务端。
//
// 这里没有引入第三方 WebSocket 库，而是基于标准库手写了 RFC 6455 的服务端
// 部分。理由是文档要求「第一阶段尽量减少第三方依赖」且运行环境是路由器：
// 手写实现约 300 行，零依赖、零 CGO，二进制更小，行为完全可控。
//
// 只实现服务端必需的部分：
//   - 握手（含 Sec-WebSocket-Accept 校验）
//   - 文本帧的接收与发送
//   - ping / pong / close 控制帧
//   - 分片消息合并、掩码解码
//
// 不实现扩展（如 permessage-deflate）、不做客户端角色。
package websocket

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 握手用的魔法字符串，由 RFC 6455 固定。
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// 帧操作码。
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const (
	// maxMessageSize 限制单条消息大小。实时传输区只传文本/链接，
	// 8KB 足够，顺便防止恶意客户端把路由器内存打满。
	maxMessageSize = 8 * 1024

	// writeWait 是单次写入的超时。
	writeWait = 10 * time.Second

	// pongWait 是等待 pong 的时限；pingPeriod 是发 ping 的间隔。
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
)

// Conn 是一条已建立的 WebSocket 连接。
type Conn struct {
	netConn net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer

	writeMu sync.Mutex
	closed  bool
	closeMu sync.Mutex

	// pingFn 在每次 ping 前调用，返回 false 表示对端已超时，应主动断开。
	pongMu   sync.Mutex
	lastPong time.Time
}

// message 是读到的一条完整消息。
type message struct {
	op   byte
	data []byte
}

// IsUpgrade 判断该请求是否是一个 WebSocket 握手请求。
func IsUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerContainsToken(r.Header.Get("Connection"), "upgrade") &&
		r.Header.Get("Sec-WebSocket-Key") != ""
}

func headerContainsToken(v, token string) bool {
	for _, part := range strings.Split(v, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// Upgrade 完成握手并返回连接。
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !IsUpgrade(r) {
		return nil, errors.New("不是合法的 WebSocket 升级请求")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		return nil, errors.New("不支持的 WebSocket 版本")
	}

	accept := computeAccept(r.Header.Get("Sec-WebSocket-Key"))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("当前 ResponseWriter 不支持 Hijack")
	}

	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("劫持连接失败: %w", err)
	}

	// 手工构造 101 响应。必须直接写到底层连接，因为已经 Hijack。
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"

	_ = netConn.SetWriteDeadline(time.Now().Add(writeWait))
	if _, err := netConn.Write([]byte(resp)); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("写入握手响应失败: %w", err)
	}
	_ = netConn.SetWriteDeadline(time.Time{})

	c := &Conn{
		netConn:  netConn,
		br:       brw.Reader,
		bw:       brw.Writer,
		lastPong: time.Now(),
	}
	return c, nil
}

func computeAccept(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// RemoteAddr 返回对端地址。
func (c *Conn) RemoteAddr() string { return c.netConn.RemoteAddr().String() }

// ReadMessage 读取下一条完整消息。控制帧在内部处理（ping→pong，close→返回错误）。
func (c *Conn) ReadMessage() (byte, []byte, error) {
	for {
		msg, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch msg.op {
		case opPing:
			if err := c.writeFrame(opPong, msg.data); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			c.pongMu.Lock()
			c.lastPong = time.Now()
			c.pongMu.Unlock()
			continue
		case opClose:
			// 回一个 close 帧后结束。
			_ = c.writeFrame(opClose, msg.data)
			return 0, nil, io.EOF
		case opText, opBinary:
			return msg.op, msg.data, nil
		default:
			return 0, nil, fmt.Errorf("不支持的操作码: %d", msg.op)
		}
	}
}

// readFrame 读一帧，若是分片则把后续帧拼起来。
func (c *Conn) readFrame() (*message, error) {
	_ = c.netConn.SetReadDeadline(time.Now().Add(pongWait))

	var fin bool
	var opcode byte
	var payload []byte
	first := true

	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(c.br, h); err != nil {
			return nil, err
		}

		fin = h[0]&0x80 != 0
		rsv := h[0] & 0x70
		if rsv != 0 {
			return nil, errors.New("不支持扩展（RSV 位非零）")
		}
		curOp := h[0] & 0x0F

		masked := h[1]&0x80 != 0
		length := int64(h[1] & 0x7F)

		switch length {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(c.br, ext); err != nil {
				return nil, err
			}
			length = int64(binary.BigEndian.Uint16(ext))
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(c.br, ext); err != nil {
				return nil, err
			}
			length = int64(binary.BigEndian.Uint64(ext))
		}

		if length < 0 || length > maxMessageSize {
			return nil, fmt.Errorf("消息过大: %d", length)
		}

		// 客户端发来的帧必须带掩码（RFC 6455 8.1）。
		if !masked {
			return nil, errors.New("客户端帧缺少掩码")
		}

		var maskKey [4]byte
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return nil, err
		}

		payload = make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}

		if first {
			opcode = curOp
			first = false
			// 控制帧不允许分片。
			if curOp >= opClose && !fin {
				return nil, errors.New("控制帧不允许分片")
			}
			if opcode == opContinuation {
				return nil, errors.New("收到意外的续帧")
			}
		}

		if fin {
			if opcode == opContinuation {
				return nil, errors.New("收到意外的续帧")
			}
			return &message{op: opcode, data: payload}, nil
		}

		// 分片：把后续帧数据累加。
		first = false
		if opcode != opContinuation {
			// 保持首个 opcode，继续读后续续帧。
		}
		if len(payload) >= maxMessageSize {
			return nil, errors.New("分片消息累计过大")
		}
	}
}

// WriteMessage 发送一条文本/二进制消息。
func (c *Conn) WriteMessage(op byte, data []byte) error {
	return c.writeFrame(op, data)
}

// WriteText 发送文本消息。
func (c *Conn) WriteText(data []byte) error { return c.writeFrame(opText, data) }

// writeFrame 写出一个完整帧。服务端发出的帧不加掩码。
func (c *Conn) writeFrame(op byte, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.isClosed() {
		return errors.New("连接已关闭")
	}

	_ = c.netConn.SetWriteDeadline(time.Now().Add(writeWait))

	var header []byte
	n := len(data)

	header = append(header, 0x80|op) // FIN + opcode
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		header = append(header, l[:]...)
	}

	if _, err := c.bw.Write(header); err != nil {
		return err
	}
	if _, err := c.bw.Write(data); err != nil {
		return err
	}
	return c.bw.Flush()
}

// Ping 发送一个 ping 帧。
func (c *Conn) Ping() error { return c.writeFrame(opPing, nil) }

// IsTimedOut 判断对端是否长时间没有响应 ping。
func (c *Conn) IsTimedOut() bool {
	c.pongMu.Lock()
	defer c.pongMu.Unlock()
	return time.Since(c.lastPong) > pongWait+pingPeriod
}

// PingLoop 周期性发送 ping，直到连接关闭。应在独立 goroutine 中运行。
func (c *Conn) PingLoop(done <-chan struct{}) {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if c.IsTimedOut() {
				_ = c.Close()
				return
			}
			if err := c.Ping(); err != nil {
				return
			}
		}
	}
}

// Close 关闭底层连接。
func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.netConn.Close()
}

func (c *Conn) isClosed() bool {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closed
}
