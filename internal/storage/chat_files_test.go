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

// TestExpiresAtColumnRemoved 验证废弃的 files.expires_at 列会被迁移移除，
// 且移除过程不丢数据、不留索引残骸。
//
// 背景：expires_at 是 temporary 文件类型的遗迹。该类型已整体移除，
// 这一列再没有读写方，因此新 schema 里不再包含它，老库则靠一次重建清除。
func TestExpiresAtColumnRemoved(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy-expires.db")

	raw, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	// 换成「含 expires_at 的旧结构」，并塞一条带过期时间的数据。
	stmts := []string{
		`DROP TABLE IF EXISTS files`,
		`CREATE TABLE files (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			original_name TEXT    NOT NULL,
			stored_name   TEXT    NOT NULL,
			size          INTEGER NOT NULL,
			sha256        TEXT    NOT NULL DEFAULT '',
			kind          TEXT    NOT NULL DEFAULT 'permanent',
			owner_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
			owner_name    TEXT    NOT NULL DEFAULT '',
			room_code     TEXT    NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER,
			pinned        INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_files_expires ON files(expires_at)`,
	}
	for _, q := range stmts {
		if _, err := raw.db.Exec(q); err != nil {
			t.Fatalf("构造旧库失败 (%s): %v", q[:28], err)
		}
	}
	// 刻意的取舍：即便 expires_at 有值（这里是一条早已过期的历史临时文件），
	// 迁移也只丢这个已无意义的过期标记，文件记录本身必须完整保留。
	if _, err := raw.db.Exec(
		`INSERT INTO files (original_name, stored_name, size, sha256, kind, owner_name, created_at, expires_at)
		 VALUES ('old-temp.bin', 'temp-stored', 99, 'cafebabe', 'permanent', 'admin', 1000, 500)`); err != nil {
		t.Fatalf("写入旧数据失败: %v", err)
	}
	// 前置条件：旧表真的有 expires_at，否则这条测试是空的。
	if has, err := raw.columnExists("files", "expires_at"); err != nil || !has {
		t.Fatalf("前置条件不成立，旧表应当有 expires_at 列 (has=%v err=%v)", has, err)
	}
	_ = raw.Close()

	// 用真正的迁移入口重开。
	s := newTestStoreAt(t, dbPath)

	// 列必须已被移除 —— 这正是本次改动的目的。
	if has, err := s.columnExists("files", "expires_at"); err != nil {
		t.Fatalf("检查列失败: %v", err)
	} else if has {
		t.Error("迁移后 files 表不该再有 expires_at 列")
	}

	// 数据不能丢：那条带过期时间的旧记录仍应完整可读。
	var name, stored, hash string
	var size int64
	if err := s.db.QueryRow(
		`SELECT original_name, stored_name, size, sha256 FROM files WHERE stored_name = 'temp-stored'`).
		Scan(&name, &stored, &size, &hash); err != nil {
		t.Fatalf("旧数据在重建后丢失: %v", err)
	}
	if name != "old-temp.bin" || size != 99 || hash != "cafebabe" {
		t.Errorf("旧数据字段不对: name=%q size=%d hash=%q", name, size, hash)
	}

	// 索引不能留残骸：DROP TABLE 会连带删掉旧索引，重建时也不该把它建回来。
	var n string
	if err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_files_expires'`).
		Scan(&n); err == nil {
		t.Error("idx_files_expires 应当随列一起消失")
	}
	// 其余索引必须仍在 —— 重建表会连带删索引，漏建就是隐性性能退化。
	for _, idx := range []string{"idx_files_kind", "idx_files_room", "idx_files_sha"} {
		if err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil {
			t.Errorf("迁移后索引 %s 应当存在: %v", idx, err)
		}
	}

	// 幂等：再跑一遍不该报错，也不该再次重建。
	if err := s.migrate(); err != nil {
		t.Fatalf("重复迁移应当幂等，却失败了: %v", err)
	}
	if has, err := s.columnExists("files", "expires_at"); err != nil || has {
		t.Errorf("二次迁移后列仍不该存在 (has=%v err=%v)", has, err)
	}

	// 迁移后写入与读取要真的能用。
	uid := mkUser(t, s, "dave")
	id := mkPermanent(t, s, "new.txt", "new-stored", "h9", uid)
	got, err := s.FileByID(id)
	if err != nil {
		t.Fatalf("迁移后写入文件失败: %v", err)
	}
	if got.OriginalName != "new.txt" {
		t.Errorf("回读文件名不对: %q", got.OriginalName)
	}
}

// TestPinnedColumnMigrates 验证「置顶」这一列能补进老库。
// 老库的 files 表没有 pinned 列。补列本身很直接，真正容易踩的坑是
// dropStoredNameUnique() 里那张手工重建的 files_new —— 它的建表语句
// 与 INSERT...SELECT 列清单是写死的，少写一列，老库重建一次就会
// 静默丢列。这里刻意造一个「既带 UNIQUE、又没有 pinned」的库，
// 让重建与补列两条路径都必须走一遍。
func TestPinnedColumnMigrates(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy-pin.db")

	raw, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	// 换成老结构：stored_name 带 UNIQUE，且没有 pinned 列。
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
			room_code     TEXT    NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_files_kind ON files(kind, created_at DESC)`,
	}
	for _, q := range stmts {
		if _, err := raw.db.Exec(q); err != nil {
			t.Fatalf("构造旧库失败 (%s): %v", q[:28], err)
		}
	}
	if _, err := raw.db.Exec(
		`INSERT INTO files (original_name, stored_name, size, sha256, kind, owner_name, created_at)
		 VALUES ('old.bin', 'old-stored', 7, 'deadbeef', 'permanent', 'admin', 1000)`); err != nil {
		t.Fatalf("写入旧数据失败: %v", err)
	}

	// 前置条件：旧表没有 pinned，且确实会触发重建。
	if has, err := raw.columnExists("files", "pinned"); err != nil || has {
		t.Fatalf("前置条件不成立，旧表不该有 pinned 列 (has=%v err=%v)", has, err)
	}
	var oldDDL string
	if err := raw.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='files'`).Scan(&oldDDL); err != nil {
		t.Fatalf("读取旧表结构失败: %v", err)
	}
	if !needsDropUnique(oldDDL) {
		t.Fatal("前置条件不成立，旧表的 stored_name 应当带 UNIQUE（否则不会走重建路径）")
	}
	_ = raw.Close()

	// 用真正的迁移入口重开。
	s := newTestStoreAt(t, dbPath)

	has, err := s.columnExists("files", "pinned")
	if err != nil {
		t.Fatalf("检查列失败: %v", err)
	}
	if !has {
		t.Fatal("迁移后 files 表应当有 pinned 列")
	}

	// 重建 + 补列之后，老数据必须一条不少、字段不丢。
	f, err := s.FileBySHA256("deadbeef", KindPermanent, 0)
	if err == ErrNotFound {
		// owner_id 为 NULL 时按 owner 查不到，退回到直接读那一行。
		var name, stored string
		var size int64
		if err := s.db.QueryRow(
			`SELECT original_name, stored_name, size FROM files WHERE stored_name = 'old-stored'`).
			Scan(&name, &stored, &size); err != nil {
			t.Fatalf("旧数据在重建后丢失: %v", err)
		}
		if name != "old.bin" || size != 7 {
			t.Errorf("旧数据字段不对: name=%q size=%d", name, size)
		}
	} else if err != nil {
		t.Fatalf("查询旧数据失败: %v", err)
	} else {
		t.Errorf("owner_id 为空的旧记录不该被 owner=0 的去重查询命中: %+v", f)
	}

	// 老数据默认不置顶，且新写入的记录也能带上这一列。
	var pinned int64
	if err := s.db.QueryRow(
		`SELECT pinned FROM files WHERE stored_name = 'old-stored'`).Scan(&pinned); err != nil {
		t.Fatalf("读取 pinned 失败: %v", err)
	}
	if pinned != 0 {
		t.Errorf("老数据的 pinned 应当默认 0，得到 %d", pinned)
	}

	// 补列之后原有的索引不能少（重建路径会连带删索引）。
	for _, idx := range []string{"idx_files_kind", "idx_files_room", "idx_files_sha"} {
		var n string
		if err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil {
			t.Errorf("迁移后索引 %s 应当存在: %v", idx, err)
		}
	}

	// 幂等：再跑一遍不该报错。
	if err := s.migrate(); err != nil {
		t.Fatalf("重复迁移应当幂等，却失败了: %v", err)
	}

	// 置顶读写要真的能用。
	uid := mkUser(t, s, "bob")
	id := mkPermanent(t, s, "pin.txt", "pin-stored", "h2", uid)
	if _, err := s.SetFilePinned(id, true); err != nil {
		t.Fatalf("置顶失败: %v", err)
	}
	got, err := s.FileByID(id)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !got.Pinned {
		t.Error("置顶后 Pinned 应为 true")
	}
}

// TestListFilesPinnedFirst 验证排序：置顶在前，其余按时间从新到旧。
func TestListFilesPinnedFirst(t *testing.T) {
	s := newTestStore(t)
	uid := mkUser(t, s, "carol")

	// 三条记录，created_at 递增：oldest < middle < newest。
	oldest := mkPermanentAt(t, s, "oldest.txt", "s1", "h1", uid, 100)
	middle := mkPermanentAt(t, s, "middle.txt", "s2", "h2", uid, 200)
	newest := mkPermanentAt(t, s, "newest.txt", "s3", "h3", uid, 300)

	// 不置顶时：新的在前。
	names := listNames(t, s)
	want := []string{"newest.txt", "middle.txt", "oldest.txt"}
	if !equalStrings(names, want) {
		t.Fatalf("默认顺序应当是从新到旧 %v，得到 %v", want, names)
	}

	// 置顶最早的那条，它必须跳到最前。
	if _, err := s.SetFilePinned(oldest, true); err != nil {
		t.Fatalf("置顶失败: %v", err)
	}
	names = listNames(t, s)
	if names[0] != "oldest.txt" {
		t.Errorf("置顶项应当排在最前，得到 %v", names)
	}

	// 再置顶一条，新的置顶项里仍是按时间从新到旧。
	if _, err := s.SetFilePinned(middle, true); err != nil {
		t.Fatalf("置顶失败: %v", err)
	}
	names = listNames(t, s)
	want = []string{"middle.txt", "oldest.txt", "newest.txt"}
	if !equalStrings(names, want) {
		t.Errorf("两个置顶项之间应按时间从新到旧，期望 %v，得到 %v", want, names)
	}

	// 取消第一条的置顶，它回到普通区的最前（时间上它最老，所以排最后）。
	if _, err := s.SetFilePinned(oldest, false); err != nil {
		t.Fatalf("取消置顶失败: %v", err)
	}
	names = listNames(t, s)
	want = []string{"middle.txt", "newest.txt", "oldest.txt"}
	if !equalStrings(names, want) {
		t.Errorf("取消置顶后期望 %v，得到 %v", want, names)
	}
	_ = newest
}

// TestRenameFileMissing 改名一个不存在的 id 返回 ErrNotFound。
func TestRenameFileMissing(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.RenameFile(12345, "x.txt"); err != ErrNotFound {
		t.Errorf("改名不存在的记录应当返回 ErrNotFound，得到 %v", err)
	}
	if _, err := s.SetFilePinned(12345, true); err != ErrNotFound {
		t.Errorf("置顶不存在的记录应当返回 ErrNotFound，得到 %v", err)
	}
}

// ---- 上面的测试用到的小工具 ----

// mkPermanentAt 像 mkPermanent 一样建记录，但显式指定 created_at（秒）。
func mkPermanentAt(t *testing.T, s *Store, name, stored, sha string, ownerID int64, createdAt int64) int64 {
	t.Helper()

	id, err := s.CreateFile(&File{
		OriginalName: name,
		StoredName:   stored,
		Size:         1,
		SHA256:       sha,
		Kind:         KindPermanent,
		OwnerID:      &ownerID,
		OwnerName:    "u",
		CreatedAt:    time.Unix(createdAt, 0),
	})
	if err != nil {
		t.Fatalf("创建永久文件记录失败: %v", err)
	}
	return id
}

// listNames 读回永久文件列表里的名字，顺序即服务端排序。
func listNames(t *testing.T, s *Store) []string {
	t.Helper()

	list, err := s.ListFiles(KindPermanent)
	if err != nil {
		t.Fatalf("列文件失败: %v", err)
	}
	names := make([]string, 0, len(list))
	for _, f := range list {
		names = append(names, f.OriginalName)
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
