package storage

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// newTestStore 建一个只在临时目录里存在的库，用完自动清理。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return newTestStoreAt(t, filepath.Join(t.TempDir(), "test.db"))
}

// newTestStoreAt 在指定路径上打开并迁移库，用于「老库升级」类测试。
func newTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreateChat(t *testing.T, s *Store, name, room string, size int64) int64 {
	t.Helper()
	id, err := s.CreateFile(&File{
		OriginalName: name,
		StoredName:   "stored-" + name,
		Size:         size,
		SHA256:       "deadbeef",
		Kind:         KindChat,
		RoomCode:     room,
		OwnerName:    "匿名设备",
		CreatedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("写入聊天文件失败: %v", err)
	}
	return id
}

// TestChatFileRoomCodePersisted 验证 room_code 列真的落库并能读回。
//
// 这是整条「房间销毁 → 文件一起删」链路的地基：
// 如果 room_code 没存进去，FilesByRoom 永远返回空，文件就再也没人删了。
func TestChatFileRoomCodePersisted(t *testing.T) {
	s := newTestStore(t)

	id := mustCreateChat(t, s, "a.txt", "ROOM1", 11)

	got, err := s.FileByID(id)
	if err != nil {
		t.Fatalf("读回文件失败: %v", err)
	}
	if got.Kind != KindChat {
		t.Errorf("kind 应为 chat，得到 %q", got.Kind)
	}
	if got.RoomCode != "ROOM1" {
		t.Errorf("room_code 应为 ROOM1，得到 %q", got.RoomCode)
	}
	if got.Size != 11 {
		t.Errorf("size 应为 11，得到 %d", got.Size)
	}
}

// TestFilesByRoom 验证按房间查文件，且不会串到别的房间。
func TestFilesByRoom(t *testing.T) {
	s := newTestStore(t)

	mustCreateChat(t, s, "r1-a.txt", "ROOM1", 1)
	mustCreateChat(t, s, "r1-b.txt", "ROOM1", 2)
	mustCreateChat(t, s, "r2-a.txt", "ROOM2", 4)

	r1, err := s.FilesByRoom("ROOM1")
	if err != nil {
		t.Fatalf("FilesByRoom 失败: %v", err)
	}
	if len(r1) != 2 {
		t.Fatalf("ROOM1 应有 2 个文件，得到 %d", len(r1))
	}
	for _, f := range r1 {
		if f.RoomCode != "ROOM1" {
			t.Errorf("混入了别的房间的文件: %q", f.RoomCode)
		}
	}

	empty, err := s.FilesByRoom("NOPE")
	if err != nil {
		t.Fatalf("FilesByRoom 失败: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("不存在的房间应返回空，得到 %d 个", len(empty))
	}

	// 仓库文件（kind=permanent）不该被 FilesByRoom 认领。
	if _, err := s.CreateFile(&File{
		OriginalName: "repo.txt", StoredName: "repo-stored", Size: 7,
		Kind: KindPermanent, RoomCode: "", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("写入仓库文件失败: %v", err)
	}
	r1b, err := s.FilesByRoom("ROOM1")
	if err != nil {
		t.Fatalf("FilesByRoom 失败: %v", err)
	}
	if len(r1b) != 2 {
		t.Errorf("仓库文件不该被计入房间文件，得到 %d 个", len(r1b))
	}
}

// TestOrphanChatFiles 验证孤儿判定：房间号不在活跃列表里的聊天文件才算孤儿。
//
// 这一条兜住的是「房间已删、文件还没来得及删」的中间态。
func TestOrphanChatFiles(t *testing.T) {
	s := newTestStore(t)

	alive := mustCreateChat(t, s, "alive.txt", "ALIVE", 3)
	dead := mustCreateChat(t, s, "dead.txt", "DEAD", 5)
	// 仓库文件永远不是孤儿。
	if _, err := s.CreateFile(&File{
		OriginalName: "repo.txt", StoredName: "repo-stored", Size: 7,
		Kind: KindPermanent, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("写入仓库文件失败: %v", err)
	}

	orphans, err := s.OrphanChatFiles([]string{"ALIVE"})
	if err != nil {
		t.Fatalf("OrphanChatFiles 失败: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("应识别出 1 个孤儿，得到 %d", len(orphans))
	}
	if orphans[0].ID != dead {
		t.Errorf("孤儿应当是 dead.txt(id=%d)，得到 id=%d", dead, orphans[0].ID)
	}
	for _, f := range orphans {
		if f.ID == alive {
			t.Error("活跃房间的文件被误判为孤儿")
		}
		if f.Kind != KindChat {
			t.Errorf("孤儿清理只应针对 chat 文件，得到 %q", f.Kind)
		}
	}

	// 房间全没了 → 两个都成孤儿。
	all, err := s.OrphanChatFiles(nil)
	if err != nil {
		t.Fatalf("OrphanChatFiles 失败: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("无活跃房间时应有 2 个孤儿，得到 %d", len(all))
	}
}

// TestDeleteFileRecord 验证删除记录后查询会返回 not found，
// 这是 purgeRoomFiles 的第二步。
func TestDeleteFileRecord(t *testing.T) {
	s := newTestStore(t)

	id := mustCreateChat(t, s, "x.txt", "ROOMX", 9)
	if err := s.DeleteFileRecord(id); err != nil {
		t.Fatalf("删除记录失败: %v", err)
	}
	if _, err := s.FileByID(id); err == nil {
		t.Error("删除后 FileByID 应当报错")
	} else if err != ErrNotFound {
		t.Errorf("应当是 ErrNotFound，得到 %v", err)
	}

	// 删完之后这个房间应当查不到文件了。
	list, err := s.FilesByRoom("ROOMX")
	if err != nil {
		t.Fatalf("FilesByRoom 失败: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("记录已删，房间应查不到文件，得到 %d 个", len(list))
	}
}

// TestRoomCodeColumnMigrates 验证「老库」（files 表没有 room_code 列）能被正确升级。
//
// 这是真实踩过的 bug：idx_files_room 原本和别的一起写在 schema 大字符串里，
// 而那段 schema 在「补列」之前执行 —— 老库执行到 CREATE INDEX 就报
// "no such column: room_code"，整个迁移中断，服务起不来。
//
// 关键：必须先造出一张**真正叫 files 且缺列**的表。
// 如果只造个别的名字（比如 legacy_files），Open() 里的
// CREATE TABLE IF NOT EXISTS files 会建出带列的新表，索引照建不误，
// 测试就永远抓不到这个顺序问题。
func TestRoomCodeColumnMigrates(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	// 第一步：手写一个「旧版本」的完整库 —— files 表没有 room_code 列。
	raw, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	// 先把已经建好的新表拆掉，换成旧结构。
	stmts := []string{
		`DROP TABLE IF EXISTS files`,
		`CREATE TABLE files (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			original_name TEXT    NOT NULL,
			stored_name   TEXT    NOT NULL UNIQUE,
			size          INTEGER NOT NULL,
			sha256        TEXT    NOT NULL DEFAULT '',
			kind          TEXT    NOT NULL DEFAULT 'permanent',
			owner_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
			owner_name    TEXT    NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_files_kind ON files(kind, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_files_expires ON files(expires_at)`,
	}
	for _, q := range stmts {
		if _, err := raw.db.Exec(q); err != nil {
			t.Fatalf("构造旧库失败 (%s): %v", q[:28], err)
		}
	}
	// 塞一条旧数据，验证重建表不会把数据弄丢。
	if _, err := raw.db.Exec(
		`INSERT INTO files (original_name, stored_name, size, sha256, kind, owner_name, created_at)
		 VALUES ('kept.bin', 'kept-stored', 42, '', 'permanent', 'admin', 1000)`); err != nil {
		t.Fatalf("写入旧数据失败: %v", err)
	}
	// 确认构造出来的确实是个缺列的老表。
	if has, err := raw.columnExists("files", "room_code"); err != nil || has {
		t.Fatalf("前置条件不成立，旧表不该有 room_code 列 (has=%v err=%v)", has, err)
	}
	// 前置条件：旧表的 stored_name 必须真的带 UNIQUE，否则这条测试是空的。
	var oldDDL string
	if err := raw.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='files'`).Scan(&oldDDL); err != nil {
		t.Fatalf("读取旧表结构失败: %v", err)
	}
	if !needsDropUnique(oldDDL) {
		t.Fatalf("前置条件不成立，旧表的 stored_name 应当带 UNIQUE:\n%s", oldDDL)
	}
	// 并且要真的会被 UNIQUE 挡住 —— 否则「需要拆约束」这个前提就无从谈起。
	if _, err := raw.db.Exec(
		`INSERT INTO files (original_name, stored_name, size, kind, owner_name, created_at)
		 VALUES ('dup.bin', 'kept-stored', 1, 'permanent', 'admin', 1001)`); err == nil {
		t.Fatal("前置条件不成立：旧表允许重复的 stored_name，UNIQUE 没生效")
	}
	_ = raw.Close()

	// 第二步：用真正的迁移入口重开。此处必须不报错 —— 修复前这里会返回
	// "执行数据库迁移失败: SQL logic error: no such column: room_code"。
	s := newTestStoreAt(t, dbPath)

	has, err := s.columnExists("files", "room_code")
	if err != nil {
		t.Fatalf("检查列失败: %v", err)
	}
	if !has {
		t.Fatal("迁移后 files 表应当有 room_code 列")
	}

	// 索引也必须真的建出来了 —— 这正是当初炸掉的那一处。
	var idxName string
	err = s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_files_room'`).
		Scan(&idxName)
	if err != nil {
		t.Fatalf("idx_files_room 未创建: %v", err)
	}

	// 去重用的内容哈希索引同样要建出来。它紧跟补列之后执行，
	// 顺序上依赖 room_code 那一段已经跑完；这里顺带把顺序回归也钉住。
	if err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_files_sha'`).
		Scan(&idxName); err != nil {
		t.Fatalf("idx_files_sha 未创建: %v", err)
	}

	// 老库里 sha256 为空的行应当被回填成 legacy:<id>，否则去重会因为
	// 空哈希而静默失效（所有空哈希互相匹配，复用一份并不存在的文件）。
	if _, err := s.db.Exec(
		`INSERT INTO files (original_name, stored_name, size, sha256, kind, owner_name, created_at)
		 VALUES ('legacy.bin', 'legacy-stored', 1, '', 'permanent', 'admin', 1)`); err != nil {
		t.Fatalf("插入空哈希旧数据失败: %v", err)
	}
	var legacyID int64
	if err := s.db.QueryRow(
		`SELECT id FROM files WHERE stored_name = 'legacy-stored'`).Scan(&legacyID); err != nil {
		t.Fatalf("读取旧数据失败: %v", err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("重复迁移应当幂等，却失败了: %v", err)
	}
	var backfilled string
	if err := s.db.QueryRow(
		`SELECT sha256 FROM files WHERE id = ?`, legacyID).Scan(&backfilled); err != nil {
		t.Fatalf("读取回填结果失败: %v", err)
	}
	if backfilled != "legacy:"+strconv.FormatInt(legacyID, 10) {
		t.Errorf("空哈希应当被回填成 legacy:<id>，得到 %q", backfilled)
	}
	// 归属未知（owner_id 为空）的旧数据不该被任何人的去重查询命中。
	if _, err := s.FileBySHA256(backfilled, KindPermanent, 1); err != ErrNotFound {
		t.Errorf("回填出来的 legacy 哈希不该命中 owner_id=1 的去重查询，得到 %v", err)
	}

	// ---- UNIQUE 约束必须已被拆掉 ----
	var newDDL string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='files'`).Scan(&newDDL); err != nil {
		t.Fatalf("读取新表结构失败: %v", err)
	}
	if needsDropUnique(newDDL) {
		t.Fatalf("迁移后 stored_name 不该再有 UNIQUE:\n%s", newDDL)
	}

	// 复用同一个存储名必须成功 —— 这是拆约束的实际效果。
	uid := mkUser(t, s, "alice")
	mkPermanent(t, s, "copy.txt", "kept-stored", "h1", uid)

	// 重建表不能把旧数据弄丢：那条 kept.bin 还在，且字段完整。
	var keptName, keptStored, keptHash string
	var keptSize int64
	if err := s.db.QueryRow(
		`SELECT original_name, stored_name, size, sha256 FROM files WHERE stored_name = 'kept-stored' AND original_name = 'kept.bin'`).
		Scan(&keptName, &keptStored, &keptSize, &keptHash); err != nil {
		t.Fatalf("旧数据在重建后丢失: %v", err)
	}
	if keptSize != 42 {
		t.Errorf("旧数据 size 应为 42，得到 %d", keptSize)
	}
	// 空哈希应当已被回填，而不是原样留着。
	if keptHash == "" {
		t.Error("旧数据的空哈希应当被回填")
	}

	// 迁移必须幂等：再跑一遍不该报错（此时列与索引都在）。
	if err := s.migrate(); err != nil {
		t.Fatalf("重复迁移应当幂等，却失败了: %v", err)
	}

	// 老库里的数据不该被迁移弄丢，且新列取默认值。
	id := mustCreateChat(t, s, "after-migrate.txt", "MIG1", 3)
	got, err := s.FileByID(id)
	if err != nil {
		t.Fatalf("迁移后写入聊天文件失败: %v", err)
	}
	if got.RoomCode != "MIG1" {
		t.Errorf("迁移后 room_code 应可用，得到 %q", got.RoomCode)
	}
}

// TestRoomCodeIndexWithoutColumn 验证「列已存在但索引缺失」的库也能被修好。
//
// 场景：上一次迁移在补列成功、建索引之前被打断。
func TestRoomCodeIndexWithoutColumn(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "half.db")

	raw, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	// 列有了，索引故意删掉，模拟「补列成功、建索引失败」的中间态。
	if _, err := raw.db.Exec(`DROP INDEX IF EXISTS idx_files_room`); err != nil {
		t.Fatalf("删索引失败: %v", err)
	}
	_ = raw.Close()

	s := newTestStoreAt(t, dbPath)

	var idxName string
	if err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_files_room'`).
		Scan(&idxName); err != nil {
		t.Fatalf("重新迁移后 idx_files_room 应被补建: %v", err)
	}
}
