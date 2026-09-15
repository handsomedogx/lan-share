package session

import (
	"testing"
	"time"
)

// TestReapDeletesExpiredRoom 验证「到点即删」——
// 这是用户需求里最关键的一条：过期了就自动删除，而不是仅仅「查不到」。
//
// 因为 reapLoop 的 ticker 是一分钟，直接等太慢；
// 这里把房间的 expiresAt 手动拨到过去，然后调用与 reapLoop 相同的删除判定。
func TestReapDeletesExpiredRoom(t *testing.T) {
	m := newManagerBare()

	r, err := m.CreateWithCode("EXPIRE", 60)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	if m.Get("EXPIRE") == nil {
		t.Fatal("刚创建的房间应当能查到")
	}

	// 拨回一小时前，模拟已过期。
	r.mu.Lock()
	r.expiresAt = time.Now().Add(-time.Hour)
	r.mu.Unlock()

	if !r.Expired() {
		t.Fatal("Expired() 应当返回 true")
	}
	if m.Get("EXPIRE") != nil {
		t.Fatal("已过期的房间 Get 应当返回 nil")
	}
	if !m.ExistsButExpired("EXPIRE") {
		t.Fatal("ExistsButExpired 应当返回 true —— 否则同名房间会被误判为「从未创建」而自动重建")
	}

	m.reapOnce(time.Now())

	if m.Get("EXPIRE") != nil {
		t.Fatal("回收后房间应当彻底消失")
	}
	if m.ExistsButExpired("EXPIRE") {
		t.Fatal("回收后同名房间号应当被释放，可以再次创建")
	}

	// 房间号释放后应能重新占用。
	if _, err := m.CreateWithCode("EXPIRE", 60); err != nil {
		t.Fatalf("过期房间号应当可以被重新占用，却失败了: %v", err)
	}
}

// TestZeroTTLRejectedByWhitelist 验证 0 分钟不再是合法档位。
//
// 0 曾表示「不限时」，现已移除：一个永不自动消失的房间既违背
// 「临时传输、用完即毁」的定位，也让聊天文件失去回收触发点
// （文件随房间销毁而删）。
func TestZeroTTLRejectedByWhitelist(t *testing.T) {
	if ValidTTL(0) {
		t.Fatal("0 分钟（曾表示不限时）必须已从白名单移除")
	}
	for _, m := range AllowedTTLMinutes {
		if m == 0 {
			t.Fatal("白名单里不该再出现 0")
		}
	}
}

// TestZeroExpiryRoomReapedWhenIdle 验证「没有到期时间」的房间仍会被空闲回收。
//
// 这条是兜底：API 已经造不出这种房间了，但只要 Room 的零值仍代表
// 「没有到期时间」，回收逻辑就必须对它明确处理 —— 否则一旦哪天有代码路径
// 漏设 expiresAt，房间和它名下的聊天文件就会永远留在磁盘上。
func TestZeroExpiryRoomReapedWhenIdle(t *testing.T) {
	m := newManagerBare()

	// 直接构造一个 expiresAt 为零值的房间，绕过 API 的白名单校验。
	r := &Room{
		Code:      "ZEROTTL",
		clients:   make(map[Client]struct{}),
		messages:  make([]Message, 0, 4),
		createdAt: time.Now(),
		lastSeen:  time.Now(),
	}
	m.mu.Lock()
	m.rooms["ZEROTTL"] = r
	m.mu.Unlock()

	if !r.ExpiresAt().IsZero() {
		t.Fatal("构造出来的房间不该有到期时间")
	}
	if r.Expired() {
		t.Fatal("没有到期时间的房间永远不该「过期」")
	}
	if got := r.TTLMinutes(); got != 0 {
		t.Fatalf("没有到期时间时 TTLMinutes 应为 0，得到 %d", got)
	}
	if got := r.RemainingSeconds(); got != -1 {
		t.Fatalf("没有到期时间时 RemainingSeconds 应为 -1，得到 %d", got)
	}

	// 房间里没人、且刚创建 —— 不该被回收。
	m.reapOnce(time.Now())
	if m.Get("ZEROTTL") == nil {
		t.Fatal("刚创建的空闲房间不该被回收")
	}

	// 空闲超过阈值后必须被回收。
	m.reapOnce(time.Now().Add(roomIdleTimeout + time.Minute))
	if m.Get("ZEROTTL") != nil {
		t.Fatal("空闲超时的无到期时间房间应当被回收 —— 否则磁盘会缓慢泄漏")
	}
}

// TestValidateCode 覆盖房间号的合法边界。
func TestValidateCode(t *testing.T) {
	good := []string{"ABCD", "ab12", "12345678", "aB3c"}
	for _, c := range good {
		c := NormalizeCode(c)
		if err := ValidateCode(c); err != nil {
			t.Errorf("%q 应当合法，却报错: %v", c, err)
		}
	}

	bad := []string{"ABC", "ABCDEFGHI", "AB-C", "AB C", "中文AB", "AB!C", ""}
	for _, c := range bad {
		if err := ValidateCode(c); err == nil {
			t.Errorf("%q 应当非法，却通过了校验", c)
		}
	}
}

// TestValidTTL 验证存活时长白名单。
func TestValidTTL(t *testing.T) {
	for _, m := range AllowedTTLMinutes {
		if !ValidTTL(m) {
			t.Errorf("白名单里的 %d 应当合法", m)
		}
	}
	// 0 曾经合法（不限时），现在必须被拒 —— 单列出来，避免以后被误加回去。
	for _, m := range []int{0, 1, 5, 7, 30, 120, 720, 2880, -1} {
		if ValidTTL(m) {
			t.Errorf("%d 不在白名单里，应当非法", m)
		}
	}
}

// TestRoomGoneHandlerFires 验证房间到期被回收时回调会被触发。
//
// 这条是「房间没了 → 聊天文件一起删」的关键接线：
// 只要回调漏触发，磁盘上的聊天文件就永远留着。
func TestRoomGoneHandlerFires(t *testing.T) {
	m := newManagerBare()

	var got []string
	m.SetRoomGoneHandler(func(code string) { got = append(got, code) })

	r, err := m.CreateWithCode("GONE1", 10)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	// 还没过期，不该触发。
	m.reapOnce(time.Now())
	if len(got) != 0 {
		t.Fatalf("未过期就触发了回调: %v", got)
	}

	// 拨到过去，再回收。
	r.mu.Lock()
	r.expiresAt = time.Now().Add(-time.Minute)
	r.mu.Unlock()

	m.reapOnce(time.Now())

	if len(got) != 1 || got[0] != "GONE1" {
		t.Fatalf("应当恰好收到一次 GONE1 回调，得到 %v", got)
	}

	// 再跑一次不该重复触发（房已不在表里）。
	m.reapOnce(time.Now())
	if len(got) != 1 {
		t.Errorf("重复回收不应再次触发回调，得到 %v", got)
	}
}

// TestNotifyGoneNilSafe 验证没有注册回调时回收不会 panic。
// 单元测试和不需要该能力的场景都会走这条路。
func TestNotifyGoneNilSafe(t *testing.T) {
	m := newManagerBare()
	r, err := m.CreateWithCode("NOCB", 10)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	r.mu.Lock()
	r.expiresAt = time.Now().Add(-time.Minute)
	r.mu.Unlock()

	m.reapOnce(time.Now()) // 不应 panic
	if m.Get("NOCB") != nil {
		t.Error("房间应当已被回收")
	}
}

// TestActiveCodes 验证活跃房间号集合 —— 孤儿清理靠它做差集。
func TestActiveCodes(t *testing.T) {
	m := newManagerBare()

	if got := m.ActiveCodes(); len(got) != 0 {
		t.Fatalf("空管理器应返回空集合，得到 %v", got)
	}

	if _, err := m.CreateWithCode("AAA1", 10); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := m.CreateWithCode("BBB2", 10); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	got := m.ActiveCodes()
	if len(got) != 2 {
		t.Fatalf("应有 2 个活跃房间，得到 %d: %v", len(got), got)
	}
	set := map[string]bool{}
	for _, c := range got {
		set[c] = true
	}
	if !set["AAA1"] || !set["BBB2"] {
		t.Errorf("活跃房间号不全: %v", got)
	}

	// 过期回收后应当从活跃集合里消失。
	r := m.Get("AAA1")
	r.mu.Lock()
	r.expiresAt = time.Now().Add(-time.Minute)
	r.mu.Unlock()
	m.reapOnce(time.Now())

	got = m.ActiveCodes()
	if len(got) != 1 || got[0] != "BBB2" {
		t.Errorf("回收后应只剩 BBB2，得到 %v", got)
	}
}

// ---------------------------------------------------------------- 过期房间号复用
//
// 以下几条覆盖本轮修复的 P0 问题：复用已过期的房间号时，
// 旧房间的资源必须先**完成清理**，新同名房间才能可见。
//
// 修复前的写法是 `delete(m.rooms, code)` 之后直接建新房间，
// 完全没调 notifyGone。后果是旧房间的聊天文件既没被删、房号又已经
// 被新房间占用，于是那些旧文件会「复活」——新房间的成员能下载到
// 上一代房间的遗留文件。

// expireRoom 把某个房间的到期时间拨到过去，模拟它已经过期。
func expireRoom(t *testing.T, m *Manager, code string) *Room {
	t.Helper()
	r := m.Get(code)
	if r == nil {
		t.Fatalf("房间 %s 不存在", code)
	}
	r.mu.Lock()
	r.expiresAt = time.Now().Add(-time.Minute)
	r.mu.Unlock()
	if !r.Expired() {
		t.Fatalf("房间 %s 应当已过期", code)
	}
	return r
}

// TestExpiredRoomReplacementNotifiesGone 是本轮修复的核心断言：
// 复用过期房间号**必须**触发旧房间的销毁回调。
func TestExpiredRoomReplacementNotifiesGone(t *testing.T) {
	m := newManagerBare()

	var gone []string
	m.SetRoomGoneHandler(func(code string) { gone = append(gone, code) })

	old, err := m.CreateWithCode("ABCD", 10)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	expireRoom(t, m, "ABCD")

	// 复用同一个房间号。
	neu, err := m.CreateWithCode("ABCD", 60)
	if err != nil {
		t.Fatalf("复用过期房间号应当成功，却失败: %v", err)
	}
	if neu == old {
		t.Fatal("应当是一个全新的 Room，而不是复用旧对象")
	}

	// 关键断言：旧的房间号必须恰好触发一次 Gone 回调。
	// 漏掉这一步，旧房间名下的聊天文件就永远不会被清理。
	if len(gone) != 1 || gone[0] != "ABCD" {
		t.Fatalf("复用过期房间号必须触发一次 ABCD 的 Gone 回调，得到 %v", gone)
	}

	// 回调必须已经完成 —— 返回新房间时不能还有清理在后台跑。
	if m.isRetiring("ABCD") {
		t.Fatal("CreateWithCode 返回时旧房间不应仍处于回收中")
	}
	if m.Get("ABCD") == nil {
		t.Fatal("新房间应当已经可见")
	}
}

// TestGoneFiredBeforeNewRoomVisible 验证「旧清理先于新房间可见」这个顺序保证。
//
// 用回调里的探针来观察时序：回调执行时，rooms 里必须还没有这个房间号；
// 回调结束后才允许出现。若顺序反了，新房间可能在清理过程中被误伤
// （清理按 room_code 删文件，会把新房间刚上传的文件一起删掉）。
func TestGoneFiredBeforeNewRoomVisible(t *testing.T) {
	m := newManagerBare()

	var visibleDuringCleanup *Room
	var stillRetiringDuringCleanup bool

	m.SetRoomGoneHandler(func(code string) {
		// 回调里刻意不做任何阻塞，只观察状态：
		// 此时这个房间号既不该在 rooms 里可见，也必须仍在 retiring 中被钉住，
		// 否则并发到来的同名创建会绕过清理窗口。
		visibleDuringCleanup = m.Get(code)
		stillRetiringDuringCleanup = m.isRetiring(code)
	})

	if _, err := m.CreateWithCode("WXYZ", 10); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	expireRoom(t, m, "WXYZ")

	if _, err := m.CreateWithCode("WXYZ", 10); err != nil {
		t.Fatalf("复用失败: %v", err)
	}

	if visibleDuringCleanup != nil {
		t.Error("清理期间旧房间不应可见")
	}
	if !stillRetiringDuringCleanup {
		t.Error("清理期间该房间号必须被 retiring 钉住，否则并发创建会插入")
	}
}

// TestRetiringBlocksConcurrentCreate 验证回收窗口内不会被抢建同名房间。
//
// 场景：清理是慢 I/O，若不加 retiring 屏障，另一个请求可以在这段时间里
// 把同名房间建出来；随后清理按 room_code 删文件就会误删新房间的数据。
func TestRetiringBlocksConcurrentCreate(t *testing.T) {
	m := newManagerBare()

	if _, err := m.CreateWithCode("RACE", 10); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	expireRoom(t, m, "RACE")

	// 在回调里（也就是清理窗口内）尝试创建同名房间，必须被拒绝。
	var innerErr error
	m.SetRoomGoneHandler(func(code string) {
		_, innerErr = m.CreateWithCode(code, 10)
	})

	if _, err := m.CreateWithCode("RACE", 10); err != nil {
		t.Fatalf("首次复用应当成功: %v", err)
	}
	if innerErr != ErrCodeTaken {
		t.Fatalf("回收窗口内创建同名房间应返回 ErrCodeTaken，得到 %v", innerErr)
	}
}

// TestGenerateCodeAvoidsRetiring 验证自动生成的房间号不会落在正在回收的号上。
func TestGenerateCodeAvoidsRetiring(t *testing.T) {
	m := newManagerBare()

	// 人为把一个房间号钉在 retiring 里。
	m.mu.Lock()
	m.retiring["FIXD"] = struct{}{}
	m.mu.Unlock()

	// GenerateCode 是随机的，单独跑一次可能撞不上；这里跑足够多次，
	// 只要实现里读了 retiring，就绝不该生成 FIXD。
	for i := 0; i < 2000; i++ {
		if code := m.GenerateCode(); code == "FIXD" {
			t.Fatal("GenerateCode 生成了正在回收中的房间号")
		}
	}
}

// TestReapOnceSkipsRetiring 验证回收协程不会重复处理正在回收的房间号。
//
// 这保证「后台回收」与「复用同名房间号」两条路径不会同时清理同一个
// room_code —— 否则回调会触发两次，第二次删的可能是新房间的文件。
func TestReapOnceSkipsRetiring(t *testing.T) {
	m := newManagerBare()

	var gone int
	m.SetRoomGoneHandler(func(string) { gone++ })

	if _, err := m.CreateWithCode("SKIP", 10); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	// 把它摘出 rooms 并标记 retiring，模拟「复用流程正卡在清理中」。
	m.mu.Lock()
	delete(m.rooms, "SKIP")
	m.retiring["SKIP"] = struct{}{}
	m.mu.Unlock()

	m.reapOnce(time.Now())

	if gone != 0 {
		t.Fatalf("正在回收的房间不该被 reapOnce 再次处理，回调触发 %d 次", gone)
	}
	if !m.isRetiring("SKIP") {
		t.Fatal("retiring 标记不该被 reapOnce 清掉")
	}
}

// TestReapOnceMarksRetiringThenClears 验证后台回收走完之后 retiring 会被清空，
// 否则这个房间号会被永久占用，永远无法再被创建。
func TestReapOnceMarksRetiringThenClears(t *testing.T) {
	m := newManagerBare()

	r, err := m.CreateWithCode("FREEME", 10)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	r.mu.Lock()
	r.expiresAt = time.Now().Add(-time.Minute)
	r.mu.Unlock()

	m.reapOnce(time.Now())

	if m.isRetiring("FREEME") {
		t.Fatal("回收完成后 retiring 应当已清空")
	}
	// 回收后房间号应当可以再次占用。
	if _, err := m.CreateWithCode("FREEME", 10); err != nil {
		t.Fatalf("回收后房间号应当可复用，却失败: %v", err)
	}
}
