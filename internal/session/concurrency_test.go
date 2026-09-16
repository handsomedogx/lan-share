package session

import (
	"sync"
	"testing"
	"time"
)

// 本文件用「并发压力 + 不变量断言」的方式覆盖竞态相关行为。
//
// 为什么不是 go test -race：本项目的开发机可能没有 C 工具链
// （-race 需要 cgo），而 ARM64 路由器上更不会跑 race。
// 这些测试在普通 go test 下也能跑，靠的是断言「无论怎么交错，
// 不变量都必须成立」，对锁的漏用同样敏感。

// TestConcurrentCreateAndReap 并发地创建、回收、读取房间，断言不会崩溃、
// 不会出现两个房间共用同一个房号。
//
// 覆盖的是 CreateWithCode 的三段式流程 + reapOnce 的 retiring 交互：
// 若清理窗口没被 retiring 钉住，这里就可能观察到「同一个 code 被建成两次」。
func TestConcurrentCreateAndReap(t *testing.T) {
	m := newManagerBare()

	var wg sync.WaitGroup
	const workers = 8
	const rounds = 200

	// 一半 worker 反复复用同一个房号，另一半反复触发回收。
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				if id%2 == 0 {
					// 不断尝试占用/复用 FIXED。
					r, err := m.CreateWithCode("FIXED", 10)
					if err == nil && r == nil {
						t.Errorf("创建成功却返回了 nil 房间")
						return
					}
					if err != nil && err != ErrCodeTaken {
						t.Errorf("只应返回 ErrCodeTaken，得到 %v", err)
						return
					}
				} else {
					// 把已存在的房间拨到过期，再让回收跑一轮。
					if r := m.Get("FIXED"); r != nil {
						r.mu.Lock()
						r.expiresAt = time.Now().Add(-time.Minute)
						r.mu.Unlock()
					}
					m.reapOnce(time.Now())
				}
			}
		}(i)
	}

	wg.Wait()

	// 收尾：所有房间号都不该仍被钉在 retiring 里，否则这个号就永久不可用了。
	m.mu.RLock()
	stuck := len(m.retiring)
	m.mu.RUnlock()
	if stuck != 0 {
		t.Errorf("并发结束后不该有房间号仍处于回收中，得到 %d 个", stuck)
	}

	// 不变量：任何时刻同一个 code 只对应一个 *Room（map 天然保证），
	// 且回收中的号不能在 rooms 里可见。
	m.mu.RLock()
	for code := range m.retiring {
		if _, visible := m.rooms[code]; visible {
			t.Errorf("房间号 %s 同时处于 retiring 且可见，状态矛盾", code)
		}
	}
	m.mu.RUnlock()
}

// TestConcurrentGenerateCodeIsSafe 并发调用 GenerateCode，覆盖它对
// rooms / retiring 两张表的读锁使用。
func TestConcurrentGenerateCodeIsSafe(t *testing.T) {
	m := newManagerBare()

	// 先塞一些占用，逼 GenerateCode 走「撞上已占用 → 重试」的分支。
	for _, c := range []string{"AAAA", "BBBB", "CCCC"} {
		if _, err := m.CreateWithCode(c, 10); err != nil {
			t.Fatalf("创建失败: %v", err)
		}
	}
	m.mu.Lock()
	m.retiring["DDDD"] = struct{}{}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				code := m.GenerateCode()
				if len(code) != codeLength {
					t.Errorf("生成的房号长度 = %d, 期望 %d", len(code), codeLength)
					return
				}
				if code == "DDDD" {
					t.Error("生成了正在回收中的房号")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentJoinLeaveAndBroadcast 并发地加入/离开/广播，
// 覆盖 Room 内部的锁使用。
func TestConcurrentJoinLeaveAndBroadcast(t *testing.T) {
	m := newManagerBare()

	room, err := m.CreateWithCode("BUSY", 60)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := &fakeClient{label: "peer"}
			for j := 0; j < 200; j++ {
				room.Join(c)
				room.Broadcast([]byte(`{"event":"x"}`), c)
				_ = room.Labels()
				_ = room.Online()
				room.Leave(c)
			}
		}(i)
	}

	// 同时有 reader 不断读历史与在线名单。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 400; j++ {
			_ = room.History()
			_ = room.Clients()
			_ = room.Labels()
		}
	}()

	wg.Wait()
}

// fakeClient 是 session.Client 的最小实现，用于并发测试。
//
// 展示名带独立锁：Join 会改写它，而广播/fan-out 会在别的 goroutine 里
// 通过 Labels() 读它 —— 不加锁就是一处于 data race。
type fakeClient struct {
	labelMu sync.RWMutex
	label   string
	mu      sync.Mutex
	got     int
}

func (c *fakeClient) Send([]byte) {
	c.mu.Lock()
	c.got++
	c.mu.Unlock()
}

func (c *fakeClient) Label() string {
	c.labelMu.RLock()
	defer c.labelMu.RUnlock()
	return c.label
}

func (c *fakeClient) SetLabel(s string) {
	c.labelMu.Lock()
	c.label = s
	c.labelMu.Unlock()
}
