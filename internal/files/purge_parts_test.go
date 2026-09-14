package files

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPurgeStaleParts(t *testing.T) {
	svc, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("创建文件服务失败: %v", err)
	}
	dir := svc.Dir("permanent")

	old := filepath.Join(dir, "111-stale.bin.part")
	fresh := filepath.Join(dir, "222-uploading.bin.part")
	done := filepath.Join(dir, "333-finished.bin")

	if err := os.WriteFile(old, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("1234567"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(done, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 把 old 的 mtime 推到 48 小时前，其余保持当前时间。
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	n, freed, err := svc.PurgeStaleParts()
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 1 {
		t.Errorf("删除了 %d 个, 期望 1", n)
	}
	if freed != 5 {
		t.Errorf("释放 %d 字节, 期望 5", freed)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("超时的 .part 没有被删掉")
	}
	// 这两条是清理逻辑的安全底线：正在传的、已经传完的都不能碰。
	if _, err := os.Stat(fresh); err != nil {
		t.Error("正在上传的 .part 被误删了")
	}
	if _, err := os.Stat(done); err != nil {
		t.Error("正式文件被误删了")
	}
}
