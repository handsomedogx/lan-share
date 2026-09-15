package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSvc 建一个指向临时目录的文件服务。
func newSvc(t *testing.T) *Service {
	t.Helper()
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("创建文件服务失败: %v", err)
	}
	return svc
}

// TestSaveHonoursPerCallLimit 验证上限是**每次调用**的参数，
// 而不是绑在 Service 上。
//
// 这是本轮解耦的核心：同一个 Service 连续两次 Save 传不同的上限，
// 各自都必须按自己的上限生效，互不影响。
func TestSaveHonoursPerCallLimit(t *testing.T) {
	svc := newSvc(t)

	// ---- 第一次：上限 4KB，传 2KB —— 应当成功 ----
	res, err := svc.Save("chat", strings.NewReader(strings.Repeat("a", 2048)), 4096)
	if err != nil {
		t.Fatalf("2KB 在 4KB 上限内应当成功，得到 %v", err)
	}
	if res.Size != 2048 {
		t.Errorf("size = %d, 期望 2048", res.Size)
	}

	// ---- 第二次：同一个 Service，上限收紧到 1KB，传 2KB —— 应当被拒 ----
	//
	// 这一条正是「上限不绑在 Service 上」的证明：若还残留 Service 级
	// maxBytes，第二次会用第一次的值而不是这里传的 1024。
	if _, err := svc.Save("chat", strings.NewReader(strings.Repeat("b", 2048)), 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过本次上限应当返回 ErrTooLarge，得到 %v", err)
	}

	// ---- 第三次：上限 0 表示不限，2KB 应当通过 ----
	res3, err := svc.Save("chat", strings.NewReader(strings.Repeat("c", 2048)), 0)
	if err != nil {
		t.Fatalf("上限 0（不限）应当成功，得到 %v", err)
	}
	if res3.Size != 2048 {
		t.Errorf("size = %d, 期望 2048", res3.Size)
	}
}

// TestSaveExactlyAtLimitAllowed 验证正好等于上限是允许的。
//
// 边界必须精确：LimitReader 读 maxBytes+1 来判断超限，
// 若判断写成 >= 就会把「刚好等于上限」的合法文件误拒。
func TestSaveExactlyAtLimitAllowed(t *testing.T) {
	svc := newSvc(t)

	const limit = 1024
	res, err := svc.Save("permanent", strings.NewReader(strings.Repeat("x", limit)), limit)
	if err != nil {
		t.Fatalf("正好等于上限应当成功，得到 %v", err)
	}
	if res.Size != limit {
		t.Errorf("size = %d, 期望 %d", res.Size, limit)
	}
}

// TestSaveTooLargeRemovesPart 验证超限时不会留下 .part 垃圾。
func TestSaveTooLargeRemovesPart(t *testing.T) {
	svc := newSvc(t)

	_, err := svc.Save("permanent", strings.NewReader(strings.Repeat("x", 4096)), 128)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应当返回 ErrTooLarge，得到 %v", err)
	}

	entries, readErr := os.ReadDir(svc.Dir("permanent"))
	if readErr != nil {
		t.Fatalf("读取目录失败: %v", readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("超限失败后不该留下任何文件，得到 %v", names)
	}
}

// TestSaveWritesToCorrectKindDir 验证两类文件各写各的目录。
func TestSaveWritesToCorrectKindDir(t *testing.T) {
	svc := newSvc(t)

	for _, kind := range []string{"permanent", "chat"} {
		res, err := svc.Save(kind, strings.NewReader("hello"), 0)
		if err != nil {
			t.Fatalf("保存到 %s 失败: %v", kind, err)
		}
		if _, err := os.Stat(filepath.Join(svc.Dir(kind), res.StoredName)); err != nil {
			t.Errorf("%s 目录里应当有刚落盘的文件: %v", kind, err)
		}
	}
}

// TestExists 验证 Exists 的判断能力 —— 去重前置检查靠它。
func TestExists(t *testing.T) {
	svc := newSvc(t)

	res, err := svc.Save("permanent", strings.NewReader("content"), 0)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	if !svc.Exists("permanent", res.StoredName) {
		t.Error("刚保存的文件应当存在")
	}
	if svc.Exists("chat", res.StoredName) {
		t.Error("换个 kind 目录查同一个存储名，不该认为存在")
	}
	if svc.Exists("permanent", "never-existed.bin") {
		t.Error("不存在的文件不该返回 true")
	}
}

// TestExistsRejectsUnsafeNames 验证非法存储名一律当作不存在。
//
// 与其去 stat 一个可疑路径（可能被用来探测目录结构），不如直接返回 false，
// 让调用方保留自己刚写下的那份好文件。
func TestExistsRejectsUnsafeNames(t *testing.T) {
	svc := newSvc(t)

	for _, bad := range []string{
		"../../etc/passwd",
		"sub/dir.bin",
		`sub\dir.bin`,
		"",
		"a:b.bin",
		strings.Repeat("z", 200),
	} {
		if svc.Exists("permanent", bad) {
			t.Errorf("非法存储名 %q 应当返回 false", bad)
		}
	}
}

// TestExistsFalseForDirectory 验证目录不算「文件存在」。
//
// 若某个存储名恰好与子目录同名，去重复用它会得到一个打不开的目标。
func TestExistsFalseForDirectory(t *testing.T) {
	svc := newSvc(t)

	dir := svc.Dir("permanent")
	if err := os.MkdirAll(filepath.Join(dir, "looks-like-file.bin"), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if svc.Exists("permanent", "looks-like-file.bin") {
		t.Error("目录不该被判定为存在的文件")
	}
}

// TestSaveComputesSHA256 验证内容哈希被正确计算 —— 去重全靠它。
func TestSaveComputesSHA256(t *testing.T) {
	svc := newSvc(t)

	a, err := svc.Save("permanent", strings.NewReader("same"), 0)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	b, err := svc.Save("permanent", strings.NewReader("same"), 0)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	c, err := svc.Save("permanent", strings.NewReader("different"), 0)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	if a.SHA256 != b.SHA256 {
		t.Error("相同内容应当得到相同哈希")
	}
	if a.SHA256 == c.SHA256 {
		t.Error("不同内容不该得到相同哈希")
	}
	if len(a.SHA256) != 64 {
		t.Errorf("sha256 应当是 64 位十六进制串，得到 %d 位", len(a.SHA256))
	}
	// 两次保存必须落在不同的物理文件上（服务层不做去重，那是上层的事）。
	if a.StoredName == b.StoredName {
		t.Error("服务层不该自行去重，存储名应当各不相同")
	}
}
