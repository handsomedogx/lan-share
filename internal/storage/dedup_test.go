package storage

import (
	"testing"
	"time"
)

// mkPermanent 写一条仓库文件记录。
//
// storedName 由调用方显式给定：去重的核心正是「多条记录共用同一个存储名」，
// 所以测试必须能自己控制这一点，不能让辅助函数偷偷生成唯一值。
func mkPermanent(t *testing.T, s *Store, name, storedName, sha string, ownerID int64) int64 {
	t.Helper()
	rec := &File{
		OriginalName: name,
		StoredName:   storedName,
		Size:         4,
		SHA256:       sha,
		Kind:         KindPermanent,
		OwnerName:    "admin",
		CreatedAt:    time.Now(),
	}
	if ownerID > 0 {
		id := ownerID
		rec.OwnerID = &id
	}
	id, err := s.CreateFile(rec)
	if err != nil {
		t.Fatalf("写入仓库文件失败: %v", err)
	}
	return id
}

// mkUser 建一个真实用户，返回自增 ID。
//
// 必须建真用户：库开着 foreign_keys(1)，owner_id 是指向 users 的外键，
// 随便编一个 ID 写进去会直接撞 FOREIGN KEY constraint failed。
func mkUser(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	u, err := s.CreateUser(name, "hash", RoleUser)
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return u.ID
}

// TestSharedStoredNameAllowed 验证多条记录可以共用同一个 stored_name。
//
// 这是内容去重的地基，也是拆掉老库 UNIQUE 约束的**唯一理由**。
// 修复前这条会报 "UNIQUE constraint failed: files.stored_name"。
//
// 拆约束的必要性：第一版把 stored_name 声明为 UNIQUE，隐含「一记录一文件」。
// 去重后一份内容只落一次盘、每人一条记录，共享存储名成了正常状态。
func TestSharedStoredNameAllowed(t *testing.T) {
	s := newTestStore(t)

	alice := mkUser(t, s, "alice")
	bob := mkUser(t, s, "bob")

	// 同一份内容的两个人，磁盘上共用一份 —— 记录各有各的原始文件名。
	idA := mkPermanent(t, s, "alice-report.pdf", "shared.bin", "samehash", alice)
	idB := mkPermanent(t, s, "bob-copy.pdf", "shared.bin", "samehash", bob)

	gotA, err := s.FileByID(idA)
	if err != nil {
		t.Fatalf("读回 alice 的记录失败: %v", err)
	}
	gotB, err := s.FileByID(idB)
	if err != nil {
		t.Fatalf("读回 bob 的记录失败: %v", err)
	}
	if gotA.StoredName != gotB.StoredName {
		t.Fatalf("两条记录应当共用存储名，得到 %q / %q", gotA.StoredName, gotB.StoredName)
	}
	// 原始文件名必须各自独立 —— 这正是「共享磁盘、私有展示」的落点。
	if gotA.OriginalName != "alice-report.pdf" || gotB.OriginalName != "bob-copy.pdf" {
		t.Errorf("原始文件名应当各自独立，得到 %q / %q",
			gotA.OriginalName, gotB.OriginalName)
	}
}

// TestFileBySHA256ScopedToOwner 验证去重查询按上传者隔离。
//
// 磁盘文件是共享的，记录却是私有的：A 不能通过「上传同一个文件」
// 探到 B 的记录，更不能把 B 的记录顶掉。
func TestFileBySHA256ScopedToOwner(t *testing.T) {
	s := newTestStore(t)

	a := mkUser(t, s, "alice")
	b := mkUser(t, s, "bob")

	const sha = "aaaa1111"
	aID := mkPermanent(t, s, "a.txt", "shared-a.bin", sha, a)
	mkPermanent(t, s, "b.txt", "shared-b.bin", sha, b)

	got, err := s.FileBySHA256(sha, KindPermanent, a)
	if err != nil {
		t.Fatalf("查自己去重记录失败: %v", err)
	}
	if got.ID != aID {
		t.Errorf("应当命中自己的记录 %d，得到 %d", aID, got.ID)
	}
	if got.StoredName != "shared-a.bin" {
		t.Errorf("存储名应为 shared-a.bin，得到 %q", got.StoredName)
	}

	// 换个人查，必须拿到他自己那份，而不是 A 的。
	gotB, err := s.FileBySHA256(sha, KindPermanent, b)
	if err != nil {
		t.Fatalf("查 B 的去重记录失败: %v", err)
	}
	if gotB.StoredName != "shared-b.bin" {
		t.Errorf("B 应当拿到自己的存储名，得到 %q", gotB.StoredName)
	}

	// 从没人上传过这份内容的人应当查不到 —— 这正是「不泄漏他人文件」的保证。
	if _, err := s.FileBySHA256(sha, KindPermanent, a+b+1000); err != ErrNotFound {
		t.Errorf("不存在的上传者应当返回 ErrNotFound，得到 %v", err)
	}
}

// TestFileBySHA256ScopedToKind 验证去重只在同类文件内发生。
//
// 这条很关键：chat 的删磁盘逻辑是「记录没了就 unlink」，
// 一旦跨类型复用了存储名，一方消亡就会把另一方的文件删掉。
func TestFileBySHA256ScopedToKind(t *testing.T) {
	s := newTestStore(t)

	uid := mkUser(t, s, "alice")

	const sha = "bbbb2222"
	mkPermanent(t, s, "repo.bin", "repo-stored.bin", sha, uid)
	mustCreateChat(t, s, "chat.bin", "ROOM1", 4)
	// 把聊天文件的哈希改成与仓库文件相同（辅助函数默认写 deadbeef）。
	if _, err := s.db.Exec(
		`UPDATE files SET sha256 = ? WHERE kind = ?`, sha, string(KindChat)); err != nil {
		t.Fatalf("改写聊天文件哈希失败: %v", err)
	}

	// 仓库侧必须查不到聊天文件，哪怕内容哈希一样。
	got, err := s.FileBySHA256(sha, KindPermanent, uid)
	if err != nil {
		t.Fatalf("仓库去重查询失败: %v", err)
	}
	if got.Kind != KindPermanent {
		t.Errorf("去重命中的必须是 permanent，得到 %q", got.Kind)
	}
	if got.StoredName != "repo-stored.bin" {
		t.Errorf("应当命中仓库文件，得到 %q", got.StoredName)
	}

	// 反过来也一样：以 chat 为条件查不到仓库文件。
	if _, err := s.FileBySHA256(sha, KindChat, uid); err != ErrNotFound {
		t.Errorf("chat 类型不该命中仓库记录，得到 %v", err)
	}
}

// TestCountFilesBySHA256 验证引用计数 —— 删除时靠它决定要不要动磁盘。
func TestCountFilesBySHA256(t *testing.T) {
	s := newTestStore(t)

	uid := mkUser(t, s, "alice")
	other := mkUser(t, s, "bob")

	const sha = "cccc3333"
	first := mkPermanent(t, s, "one.txt", "shared.bin", sha, uid)
	second := mkPermanent(t, s, "two.txt", "shared.bin", sha, uid)
	// 同一个存储名，但属于别人 —— 不该计入当前上传者的引用数。
	mkPermanent(t, s, "other.txt", "other.bin", sha, other)

	n, err := s.CountFilesBySHA256(sha, KindPermanent, uid)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("上传者 alice 应有 2 条引用，得到 %d", n)
	}

	// 删掉一条，仍剩 1 条 —— 此时绝不能 unlink 共享的 shared.bin。
	if err := s.DeleteFileRecord(first); err != nil {
		t.Fatalf("删除记录失败: %v", err)
	}
	if n, err = s.CountFilesBySHA256(sha, KindPermanent, uid); err != nil || n != 1 {
		t.Fatalf("删一条后应剩 1 条引用，得到 %d (err=%v)", n, err)
	}

	// 删掉最后一条，计数归零 —— 这时才轮到删磁盘。
	if err := s.DeleteFileRecord(second); err != nil {
		t.Fatalf("删除记录失败: %v", err)
	}
	if n, err = s.CountFilesBySHA256(sha, KindPermanent, uid); err != nil || n != 0 {
		t.Fatalf("全部删除后引用应归零，得到 %d (err=%v)", n, err)
	}
}

// TestCountFilesByStoredName 验证「按磁盘对象名」的引用计数 ——
// 这是删除时真正决定要不要 unlink 的那个口径。
//
// 与 CountFilesBySHA256 的关键差别：那个口径把引用数限定在
// 「同一内容 + 同一类型 + 同一上传者」这个三元组上，而磁盘上被删的
// 物理对象是 stored_name。问「还有没有记录指向它」才是最直接、最不会漏判的。
func TestCountFilesByStoredName(t *testing.T) {
	s := newTestStore(t)

	alice := mkUser(t, s, "alice")
	bob := mkUser(t, s, "bob")

	const stored = "shared-physical.bin"

	// 两条记录共享同一个磁盘对象（不同上传者、不同原始文件名）。
	first := mkPermanent(t, s, "alice-report.pdf", stored, "hash-a", alice)
	second := mkPermanent(t, s, "bob-copy.pdf", stored, "hash-b", bob)

	if n, err := s.CountFilesByStoredName(stored); err != nil || n != 2 {
		t.Fatalf("共享存储名应有 2 条引用，得到 %d (err=%v)", n, err)
	}

	// 删掉第一条：物理文件仍被第二条引用，计数必须是 1，不能 unlink。
	if err := s.DeleteFileRecord(first); err != nil {
		t.Fatalf("删除记录失败: %v", err)
	}
	if n, err := s.CountFilesByStoredName(stored); err != nil || n != 1 {
		t.Fatalf("删一条后应剩 1 条引用，得到 %d (err=%v)", n, err)
	}

	// 删掉最后一条：计数归零，这时才允许删磁盘文件。
	if err := s.DeleteFileRecord(second); err != nil {
		t.Fatalf("删除记录失败: %v", err)
	}
	if n, err := s.CountFilesByStoredName(stored); err != nil || n != 0 {
		t.Fatalf("全部删除后引用应归零，得到 %d (err=%v)", n, err)
	}

	// 不存在的存储名应当返回 0 而不是报错 —— 调用方据此安全地走删除分支。
	if n, err := s.CountFilesByStoredName("never-existed.bin"); err != nil || n != 0 {
		t.Fatalf("不存在的存储名应返回 0，得到 %d (err=%v)", n, err)
	}
}

// TestCountFilesByStoredNameIgnoresKindAndOwner 验证按存储名计数时
// **不**附加 kind / owner 条件。
//
// 理由：这条查询要回答的是「磁盘上这个文件还有没有人引用」，
// 与引用它的记录属于谁、是什么类型无关。放宽条件只可能更安全
// （多留一个孤儿文件）而不会更危险（误删别人的数据）。
func TestCountFilesByStoredNameIgnoresKindAndOwner(t *testing.T) {
	s := newTestStore(t)

	alice := mkUser(t, s, "alice")

	const stored = "cross-kind.bin"
	mkPermanent(t, s, "repo.bin", stored, "h1", alice)

	// 手工插一条 owner_id 为 NULL 的记录指向同一个存储名。
	// 去重查询（要求 owner 相等）永远命中不到它，但它是真实存在的引用，
	// 按存储名计数必须把它算进去。
	if _, err := s.db.Exec(
		`INSERT INTO files (original_name, stored_name, size, sha256, kind, owner_name, created_at)
		 VALUES ('orphan.bin', ?, 4, 'h2', 'permanent', '', 1000)`, stored); err != nil {
		t.Fatalf("插入无主记录失败: %v", err)
	}

	n, err := s.CountFilesByStoredName(stored)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("无主记录也应计入引用，期望 2 得到 %d", n)
	}

	// 对照：按 sha256 + owner 的旧口径只认 alice 那一条。
	if n2, err := s.CountFilesBySHA256("h1", KindPermanent, alice); err != nil || n2 != 1 {
		t.Fatalf("旧口径应只统计到 1 条，得到 %d (err=%v)", n2, err)
	}
}
