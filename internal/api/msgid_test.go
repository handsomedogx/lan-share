package api

import (
	"sync"
	"testing"
)

// TestNewMsgIDConcurrent 并发生成消息 ID，断言不产生重复。
//
// 这就是 msgSeq 从裸 uint64 改成 atomic.Uint64 要解决的问题：
// 这个函数会被并发的 HTTP / WebSocket handler 调用（多个房间、多个连接
// 同时发消息），裸的 msgSeq++ 是 data race —— 在 -race 下会直接报，
// 在高并发下则可能读到同一个值，产生重复 ID。
//
// 用「结果去重后数量是否等于调用次数」来断言：即便没有 race detector，
// 真实发生的丢失更新也能让这条测试失败。
func TestNewMsgIDConcurrent(t *testing.T) {
	const (
		workers = 16
		perW    = 500
	)

	ids := make(chan string, workers*perW)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perW; j++ {
				ids <- newMsgID()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, workers*perW)
	for id := range ids {
		if id == "" {
			t.Fatal("消息 ID 不该为空")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("消息 ID 重复: %s", id)
		}
		seen[id] = struct{}{}
	}

	want := workers * perW
	if len(seen) != want {
		t.Fatalf("应当生成 %d 个唯一 ID，得到 %d（说明计数器丢过更新）", want, len(seen))
	}
}
