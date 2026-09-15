package websocket

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 测试脚手架
//
// 这些测试直接构造**原始字节**喂给 Conn，而不是走完整的 HTTP 握手。
// 理由：本轮修复的是帧/消息层的协议状态机（分片拼接、控制帧插入、
// 非法序列拒绝），把它和握手搅在一起测，定位失败原因会困难得多。
//
// fakeConn 只实现 net.Conn 的最小可满足子集；写入方向被丢弃，
// 因为协议状态机只关心读取方向。

type fakeConn struct {
	r *bytes.Reader
	net.Conn
}

func (c *fakeConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *fakeConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *fakeConn) Close() error                { return nil }
func (c *fakeConn) LocalAddr() net.Addr         { return nil }
func (c *fakeConn) RemoteAddr() net.Addr        { return nil }
func (c *fakeConn) SetDeadline(time.Time) error { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error {
	return nil
}
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// newTestConn 用给定的原始字节构造一个待读的 Conn。
func newTestConn(raw []byte) *Conn {
	fc := &fakeConn{r: bytes.NewReader(raw)}
	return &Conn{
		netConn:  fc,
		br:       bufio.NewReader(fc),
		bw:       bufio.NewWriter(io.Discard),
		lastPong: time.Now(),
	}
}

// ---------------------------------------------------------------- 帧编码
//
// 客户端发出的帧必须带掩码，所以测试侧要自己编码掩码。
// maskKey 固定用 0x01..0x04，便于人工核对解码结果。

var testMask = [4]byte{0x01, 0x02, 0x03, 0x04}

// encodeFrame 把一段 payload 编成一个客户端帧。
//
// fin / opcode 由调用方指定，这样才能构造分片与控制帧的各种组合。
func encodeFrame(fin bool, opcode byte, payload []byte) []byte {
	var b []byte

	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	b = append(b, b0)

	n := len(payload)
	switch {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n <= 0xFFFF:
		b = append(b, 0x80|126, byte(n>>8), byte(n))
	default:
		b = append(b, 0x80|127)
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		b = append(b, l[:]...)
	}

	b = append(b, testMask[:]...)
	for i, c := range payload {
		b = append(b, c^testMask[i%4])
	}
	return b
}

// encodeFrameNoMask 编一个**不带掩码**的帧，用于验证服务端会拒绝它。
func encodeFrameNoMask(fin bool, opcode byte, payload []byte) []byte {
	var b []byte
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	b = append(b, b0, byte(len(payload)))
	return append(b, payload...)
}

// ---------------------------------------------------------------- 分片基础

// TestSingleFrameText 是最基本的回归：单帧消息必须原样读出。
func TestSingleFrameText(t *testing.T) {
	c := newTestConn(encodeFrame(true, opText, []byte("hello")))

	op, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if op != opText {
		t.Errorf("opcode = %d, 期望 %d", op, opText)
	}
	if string(data) != "hello" {
		t.Errorf("内容 = %q, 期望 hello", data)
	}
}

// TestFragmentedTextTwoParts 是本轮修复的**核心断言**。
//
// 修复前 readFrame 每轮循环都 `payload = make([]byte, length)`，
// 于是 "hel" + "lo" 最终只返回最后一片 "lo"。正确结果必须是 "hello"。
func TestFragmentedTextTwoParts(t *testing.T) {
	raw := append(encodeFrame(false, opText, []byte("hel")),
		encodeFrame(true, opContinuation, []byte("lo"))...)
	c := newTestConn(raw)

	op, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if op != opText {
		t.Errorf("opcode 应当是首帧的 text(%d)，得到 %d", opText, op)
	}
	if string(data) != "hello" {
		t.Fatalf("分片消息应当拼成 %q，得到 %q", "hello", data)
	}
}

// TestFragmentedTextThreeParts 验证三片消息同样能正确拼接。
func TestFragmentedTextThreeParts(t *testing.T) {
	raw := append(encodeFrame(false, opText, []byte("a")),
		encodeFrame(false, opContinuation, []byte("b"))...)
	raw = append(raw, encodeFrame(true, opContinuation, []byte("c"))...)
	c := newTestConn(raw)

	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "abc" {
		t.Fatalf("三片消息应当拼成 abc，得到 %q", data)
	}
}

// TestBinaryFragmented 验证二进制消息的分片同样走拼接路径，且 opcode 保持 binary。
func TestBinaryFragmented(t *testing.T) {
	raw := append(encodeFrame(false, opBinary, []byte{1, 2}),
		encodeFrame(true, opContinuation, []byte{3, 4})...)
	c := newTestConn(raw)

	op, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if op != opBinary {
		t.Errorf("opcode = %d, 期望 binary(%d)", op, opBinary)
	}
	if !bytes.Equal(data, []byte{1, 2, 3, 4}) {
		t.Errorf("内容 = %v, 期望 [1 2 3 4]", data)
	}
}

// TestTwoMessagesInSequence 验证一条消息读完不会吃掉下一条的帧。
func TestTwoMessagesInSequence(t *testing.T) {
	raw := append(encodeFrame(true, opText, []byte("one")),
		encodeFrame(true, opText, []byte("two"))...)
	c := newTestConn(raw)

	for _, want := range []string{"one", "two"} {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("读取 %q 失败: %v", want, err)
		}
		if string(data) != want {
			t.Errorf("内容 = %q, 期望 %q", data, want)
		}
	}
}

// ---------------------------------------------------------------- 控制帧

// TestControlFrameInMiddleOfFragmented 验证 ping 插在分片消息中间
// 不会破坏拼接，也不会被当成 continuation。
//
// RFC 6455 明确允许控制帧插在分片消息中间（控制帧必须 FIN=1 且
// payload <= 125）。修复前若在拼接循环里处理控制帧，很容易把它
// 误算进 payload，导致最终消息带上一段垃圾。
func TestControlFrameInMiddleOfFragmented(t *testing.T) {
	raw := append(encodeFrame(false, opText, []byte("hel")),
		encodeFrame(true, opPing, []byte("ping!"))...)
	raw = append(raw, encodeFrame(true, opContinuation, []byte("lo"))...)
	c := newTestConn(raw)

	op, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if op != opText {
		t.Errorf("opcode = %d, 期望 text(%d)", op, opText)
	}
	if string(data) != "hello" {
		t.Fatalf("插入 ping 后仍应得到 hello，得到 %q", data)
	}
}

// TestPongInMiddleOfFragmented 验证 pong 同样不干扰拼接。
func TestPongInMiddleOfFragmented(t *testing.T) {
	raw := append(encodeFrame(false, opText, []byte("ab")),
		encodeFrame(true, opPong, nil)...)
	raw = append(raw, encodeFrame(true, opContinuation, []byte("cd"))...)
	c := newTestConn(raw)

	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "abcd" {
		t.Fatalf("插入 pong 后仍应得到 abcd，得到 %q", data)
	}
}

// TestCloseReturnsEOF 验证 close 帧会让 ReadMessage 以 io.EOF 结束。
func TestCloseReturnsEOF(t *testing.T) {
	c := newTestConn(encodeFrame(true, opClose, nil))

	if _, _, err := c.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("close 帧应返回 io.EOF，得到 %v", err)
	}
}

// TestControlFrameFINZeroRejected 验证控制帧带 FIN=0 会被拒绝。
//
// 控制帧不允许分片，这是 RFC 6455 5.5 的硬要求。
func TestControlFrameFINZeroRejected(t *testing.T) {
	c := newTestConn(encodeFrame(false, opPing, []byte("x")))

	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("FIN=0 的控制帧应当被拒绝")
	}
	if !strings.Contains(err.Error(), "控制帧") {
		t.Errorf("错误信息应当说明是控制帧问题，得到 %v", err)
	}
}

// TestControlFrameTooLongRejected 验证控制帧 payload 超过 125 字节会被拒绝。
func TestControlFrameTooLongRejected(t *testing.T) {
	c := newTestConn(encodeFrame(true, opPing, bytes.Repeat([]byte("x"), 126)))

	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("payload 126 字节的控制帧应当被拒绝")
	}
	if !strings.Contains(err.Error(), "控制帧") {
		t.Errorf("错误信息应当说明是控制帧问题，得到 %v", err)
	}
}

// TestControlFrame125Allowed 验证 125 字节正好是允许的上界。
func TestControlFrame125Allowed(t *testing.T) {
	raw := append(encodeFrame(true, opPing, bytes.Repeat([]byte("x"), 125)),
		encodeFrame(true, opText, []byte("ok"))...)
	c := newTestConn(raw)

	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("125 字节的控制帧应当被接受，得到 %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("内容 = %q, 期望 ok", data)
	}
}

// ---------------------------------------------------------------- 非法序列

// TestFirstFrameContinuationRejected 验证首帧就是 continuation 会被拒绝。
//
// 没有起始的 text/binary 帧，就无从知道这条消息的类型。
func TestFirstFrameContinuationRejected(t *testing.T) {
	c := newTestConn(encodeFrame(true, opContinuation, []byte("orphan")))

	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("首帧为 continuation 应当被拒绝")
	}
	if !strings.Contains(err.Error(), "续帧") {
		t.Errorf("错误信息应当说明是续帧问题，得到 %v", err)
	}
}

// TestSecondDataFrameNotContinuationRejected 验证分片未结束时
// 又出现新的 text/binary 帧会被拒绝。
func TestSecondDataFrameNotContinuationRejected(t *testing.T) {
	raw := append(encodeFrame(false, opText, []byte("first")),
		encodeFrame(true, opText, []byte("second"))...)
	c := newTestConn(raw)

	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("分片未结束时出现新的数据帧应当被拒绝")
	}
	if !strings.Contains(err.Error(), "分片") && !strings.Contains(err.Error(), "数据帧") {
		t.Errorf("错误信息应当说明是分片序列问题，得到 %v", err)
	}
}

// TestFrameWithoutMaskRejected 验证没有掩码的客户端帧会被拒绝。
func TestFrameWithoutMaskRejected(t *testing.T) {
	c := newTestConn(encodeFrameNoMask(true, opText, []byte("nomask")))

	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("缺少掩码的帧应当被拒绝")
	}
	if !strings.Contains(err.Error(), "掩码") {
		t.Errorf("错误信息应当说明是掩码问题，得到 %v", err)
	}
}

// TestRsvBitsRejected 验证 RSV 位非零会被拒绝（本实现不支持扩展）。
func TestRsvBitsRejected(t *testing.T) {
	raw := encodeFrame(true, opText, []byte("x"))
	raw[0] |= 0x40 // 置上 RSV1

	c := newTestConn(raw)
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("RSV 位非零应当被拒绝")
	}
}

// TestUnknownOpcodeRejected 验证未知操作码会被拒绝。
func TestUnknownOpcodeRejected(t *testing.T) {
	c := newTestConn(encodeFrame(true, 0x3, []byte("x")))

	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("未知操作码应当被拒绝")
	}
}

// ---------------------------------------------------------------- 大小限制

// TestCumulativeSizeLimit 验证分片累计超过上限会被拒绝。
//
// 这是分片引入的**新**攻击面：单帧都不超限，但总量可以无上限。
// 不接受这条检查，一个客户端就能用无数个小分片把路由器内存吃干。
func TestCumulativeSizeLimit(t *testing.T) {
	chunk := bytes.Repeat([]byte("a"), 1024)

	// 先来 8 片 1KB（首帧 + 7 片续帧），距离 8KB 上限还差一点。
	var raw []byte
	raw = append(raw, encodeFrame(false, opText, chunk)...)
	for i := 0; i < 7; i++ {
		raw = append(raw, encodeFrame(false, opContinuation, chunk)...)
	}
	// 第 9 片会让累计达到 9216 字节 > 8192，必须被拒。
	raw = append(raw, encodeFrame(true, opContinuation, chunk)...)

	c := newTestConn(raw)
	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("累计超过 maxMessageSize 的分片消息应当被拒绝")
	}
	if !strings.Contains(err.Error(), "累计过大") {
		t.Errorf("错误信息应当说明累计过大，得到 %v", err)
	}
}

// TestExactlyAtSizeLimitAllowed 验证累计正好等于上限是允许的。
//
// 边界必须精确：拒绝「等于上限」会让合法的大消息莫名失败，
// 而本服务端的上限就是 8KB，正好卡在边界上的消息并不罕见。
func TestExactlyAtSizeLimitAllowed(t *testing.T) {
	half := bytes.Repeat([]byte("a"), maxMessageSize/2)

	raw := append(encodeFrame(false, opText, half),
		encodeFrame(true, opContinuation, half)...)
	c := newTestConn(raw)

	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("正好等于上限的消息应当被接受，得到 %v", err)
	}
	if len(data) != maxMessageSize {
		t.Errorf("长度 = %d, 期望 %d", len(data), maxMessageSize)
	}
}

// TestSingleFrameTooLargeRejected 验证单帧超过上限会被拒绝
// （这一条在分片逻辑引入前就存在，保留以防回归）。
func TestSingleFrameTooLargeRejected(t *testing.T) {
	c := newTestConn(encodeFrame(true, opText, bytes.Repeat([]byte("x"), maxMessageSize+1)))

	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("超过 maxMessageSize 的单帧应当被拒绝")
	}
}

// ---------------------------------------------------------------- 掩码解码

// TestMaskDecodingRoundTrip 用一段包含各种字节值的 payload 验证掩码解码正确。
//
// 掩码是 a^b 的自反运算，encodeFrame 与解码用了同一个 key，
// 所以「编码再解码得到原文」能同时覆盖两边的实现。
func TestMaskDecodingRoundTrip(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x7F, 0x80, 0xFF, 'h', 'i'}
	c := newTestConn(encodeFrame(true, opBinary, payload))

	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("解码结果 = %v, 期望 %v", data, payload)
	}
}

// TestExtendedLength126 验证 126 长度形态（16 位）能正确解析。
func TestExtendedLength126(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 300) // > 125，触发 126 形态
	c := newTestConn(encodeFrame(true, opBinary, payload))

	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("长度 %d 的内容不匹配", len(data))
	}
}
