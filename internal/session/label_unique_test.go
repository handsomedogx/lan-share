package session

import "testing"

// TestJoinMakesLabelsUnique 验证同名连接在房间内被唯一化。
//
// 这不是假想的边角场景：展示名默认由客户端 IP 兜底，而
// 「同一台机器开两个标签页」和「所有设备都经由同一个反向代理进来」
// 都会让 base 名完全相同。不唯一化的话，在线列表会把多个人合成一条
// （Labels 去重），前端也就无从回答「这条消息是谁发的」。
func TestJoinMakesLabelsUnique(t *testing.T) {
	m := newManagerBare()
	room, err := m.CreateWithCode("SAME", 60)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	a := &fakeClient{label: "192.168.1.5"}
	b := &fakeClient{label: "192.168.1.5"}
	c := &fakeClient{label: "192.168.1.5"}
	d := &fakeClient{label: "192.168.1.9"}

	want := []struct {
		c     *fakeClient
		label string
	}{
		{a, "192.168.1.5"},
		{b, "192.168.1.5 #2"},
		{c, "192.168.1.5 #3"},
		{d, "192.168.1.9"}, // 不同名不受影响
	}
	for _, w := range want {
		if got := room.Join(w.c); got != w.label {
			t.Errorf("Join 返回名 = %q，期望 %q", got, w.label)
		}
		// 连接自身的 Label 必须同步成最终名：
		// 消息里的 sender 就是从这里取的，不同步就还是旧名。
		if got := w.c.Label(); got != w.label {
			t.Errorf("连接 Label = %q，期望 %q", got, w.label)
		}
	}

	if got := len(room.Labels()); got != 4 {
		t.Errorf("在线名列表应有 4 项，实际 %d 项: %v", got, room.Labels())
	}

	// 离开后名字要释放，后来者可以复用 —— 否则一次重连就把序号推到天上。
	room.Leave(b)
	e := &fakeClient{label: "192.168.1.5"}
	if got := room.Join(e); got != "192.168.1.5 #2" {
		t.Errorf("离开后 #2 应当可复用，实际拿到 %q", got)
	}
}
