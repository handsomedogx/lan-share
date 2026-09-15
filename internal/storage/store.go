// Package storage 负责 SQLite 的打开、迁移与全部数据访问。
//
// 选型说明：使用 modernc.org/sqlite（Pure Go 实现），不依赖 CGO / glibc，
// 这样在 CGO_ENABLED=0 的交叉编译下可以直接产出独立的 Linux ARM64 ELF，
// 非常适合 Kwrt/OpenWrt 路由器。
//
// 数据划分原则：文件本体放文件系统，SQLite 只存元数据。
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// Store 封装数据库连接。
type Store struct {
	db *sql.DB
}

// Open 打开数据库并执行迁移。
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("数据库路径为空")
	}

	// 使用 WAL 提升并发读性能，busy_timeout 避免偶发 SQLITE_BUSY。
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"

	// 只是为了让非法路径尽早暴露。
	if _, err := url.Parse(dsn); err != nil {
		return nil, fmt.Errorf("数据库 DSN 非法: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 路由器内存紧张，同时并发写很少，因此限制连接数，
	// 顺便规避 SQLite 单写者模型下的锁竞争。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// migrate 创建基础表。使用 IF NOT EXISTS，可重复执行。
//
// 除了建表，还负责「给老库补列」：第一版只有 users/sessions/files 三张表，
// 管理员与注册开关是后加的，因此对已存在的库必须做增量升级，
// 不能指望用户删库重来（路由器上那是真实数据）。
func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    original_name TEXT   NOT NULL,
    -- 注意：这里刻意没有 UNIQUE。
    --
    -- stored_name 是「磁盘上的那份数据」。内容去重后，同一份内容只落一次盘，
    -- 但每个上传者都有自己的一条记录（各自的原始文件名、各自能删自己的）——
    -- 于是多条记录指向同一个 stored_name 就是正常状态。
    -- 老库里这条约束是 UNIQUE，由下面的迁移拆除。
    stored_name  TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL DEFAULT 'permanent',
    owner_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    owner_name   TEXT    NOT NULL DEFAULT '',
    room_code    TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    -- 置顶：0 普通，1 置顶。用整数而不是布尔，是因为 SQLite 的布尔本身就是
    -- INTEGER 的别名，直接存 0/1 更省事，也能在 ORDER BY 里直接参与排序。
    pinned       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_files_kind ON files(kind, created_at DESC);
-- 注意：idx_files_room 不在这里建。
-- 老库的 files 表没有 room_code 列，而这一段 schema 会在「补列」之前执行，
-- 直接 CREATE INDEX 会报 "no such column: room_code" 并中断整个迁移。
-- 它被挪到下面的 ALTER TABLE 之后执行。

-- 键值配置表。用来存「注册是否开放」这类运行时开关，
-- 比塞进代码常量灵活：管理员点一下就能改，不用重启。
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("执行数据库迁移失败: %w", err)
	}

	// 增量升级：老库的 users 表没有 role 列。
	hasRole, err := s.columnExists("users", "role")
	if err != nil {
		return err
	}
	if !hasRole {
		if _, err := s.db.Exec(
			`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'`); err != nil {
			return fmt.Errorf("为用户表添加 role 列失败: %w", err)
		}
		// 补列后立刻做一次「至少有一个管理员」的兜底：
		// 老库里的用户都是第一版建的，role 全是 user，会导致
		// 「谁都不能管理文件」的死局。把最早的那个提升为管理员。
		if _, err := s.db.Exec(`
			UPDATE users SET role = 'admin'
			WHERE id = (SELECT MIN(id) FROM users)
			  AND NOT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`); err != nil {
			return fmt.Errorf("初始化管理员角色失败: %w", err)
		}
	}

	// 增量升级：老库的 files 表没有 room_code 列（聊天室文件是后加的能力）。
	hasRoom, err := s.columnExists("files", "room_code")
	if err != nil {
		return err
	}
	if !hasRoom {
		if _, err := s.db.Exec(
			`ALTER TABLE files ADD COLUMN room_code TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("为文件表添加 room_code 列失败: %w", err)
		}
	}
	// 索引统一在这里建，且不放在 if 里面 —— 这样「列已存在但索引没建」的库
	// （比如上一次迁移恰好卡在别处）也能被修好。CREATE INDEX IF NOT EXISTS 幂等。
	if _, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_files_room ON files(room_code)`); err != nil {
		return fmt.Errorf("为文件表创建房间索引失败: %w", err)
	}

	// 去重查询走 sha256 + kind + owner_id 三列，建复合索引避免全表扫。
	// sha256 列是第一版就有的，此处不需要补列，因此可以紧跟其后。
	if _, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_files_sha ON files(sha256, kind, owner_id)`); err != nil {
		return fmt.Errorf("为文件表创建内容哈希索引失败: %w", err)
	}

	// 增量升级：老库的 files 表没有 pinned 列（置顶是后加的能力）。
	if err := s.ensureFilePinnedColumn(); err != nil {
		return err
	}

	// 增量升级：拆掉 stored_name 上的 UNIQUE 约束。
	//
	// 第一版把 stored_name 声明成 UNIQUE（一条记录一个磁盘文件），
	// 内容去重后这个假设不再成立：同一份内容只落一次盘，但每个上传者
	// 各有一条记录，多条记录共用同一个 stored_name 是正常状态。
	//
	// SQLite 不支持 DROP CONSTRAINT，只能重建表。这里用 12 步 alter 的
	// 标准做法，且整体包在事务里 —— 中途失败必须完整回滚，绝不能留下
	// 一张缺数据的 files 表（那是用户真实的文件元数据）。
	if err := s.dropStoredNameUnique(); err != nil {
		return err
	}

	// 补列兜底：极老的库（stored_name 仍带 UNIQUE）会在上面那次重建里
	// 按 files_new 的列定义重建，pinned 那时就已经在了；但为了不让
	// 「补列」依赖「重建」这个副作用，这里再确认一次并在缺列时补上。
	// columnExists 是幂等的，重复调用没有代价。
	//
	// 顺序也说得通：重建在先、补列在后，无论从哪条路径进来，
	// 出了 migrate 的 files 表一定同时具备 latest schema 的全部列。
	if err := s.ensureFilePinnedColumn(); err != nil {
		return err
	}

	// 一次性补齐历史数据的 sha256。
	//
	// 放在重建表之后：重建是 COPY，把空哈希原样搬了过去，这里统一回填。
	// 正常情况下不会命中任何行（第一版 Save 就已经在算哈希了），它只是
	// 兜底「万一有历史遗留的空哈希」导致去重整条链路静默失效 ——
	// 空哈希会互相全匹配，让去重复用一份内容完全对不上的文件。
	// 回填值同样不可能等于真实文件的哈希，因此这类旧数据天然不会被误复用。
	if _, err := s.db.Exec(
		`UPDATE files SET sha256 = 'legacy:' || id WHERE sha256 = ''`); err != nil {
		return fmt.Errorf("回填文件内容哈希失败: %w", err)
	}

	// 增量升级：移除已废弃的 expires_at 列（temporary 文件类型的遗迹）。
	//
	// 放在最后执行，因为它要重建表：上面几步（补 pinned、拆 UNIQUE、回填 sha256）
	// 都建立在「表结构仍含 expires_at」的基础上，顺序颠倒会让它们读到不同的列集。
	// 拆除后 files 表的列定义与 schema 中的定义完全一致。
	if err := s.dropExpiresAtColumn(); err != nil {
		return err
	}

	return nil
}

// dropExpiresAtColumn 若 files 表仍有 expires_at 列，则重建表去掉它。
//
// 背景：temporary 类型按 expires_at 到期自动删除，该类型已被整体移除，
// 这一列再没有任何读写方，留着只是 schema 噪音与维护负担。
//
// SQLite 不支持 DROP COLUMN 的历史较早，这里沿用与 dropStoredNameUnique
// 相同的「12 步 alter」重建法，整体包在事务里 —— files 是用户真实的文件
// 元数据，中途失败必须完整回滚。
//
// 幂等：列不存在时直接返回。这也让本方法天然成为「重建路径的兜底」——
// 重建 SQL 本就不含 expires_at，因此极老的库经由 dropStoredNameUnique
// 重建后，这里会因列已消失而空跑。
//
// 注意一个刻意的取舍：**这一步不判断"列里有没有非空数据"**。
// expires_at 若有值，其含义是「这个临时文件何时该被删」——而 temporary
// 类型已不被写入，残留值只可能来自早已失效的历史数据；此时正确的动作是
// 保留文件本体、丢弃这个已无意义的过期标记，而不是为了保住一列死数据
// 就让整个 schema 永远背着一个废弃字段。
func (s *Store) dropExpiresAtColumn() error {
	has, err := s.columnExists("files", "expires_at")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmts := []string{
		`CREATE TABLE files_new (
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
			pinned        INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO files_new (id, original_name, stored_name, size, sha256, kind,
		                        owner_id, owner_name, room_code, created_at, pinned)
		 SELECT id, original_name, stored_name, size, sha256, kind,
		        owner_id, owner_name, room_code, created_at, pinned FROM files`,
		`DROP TABLE files`,
		`ALTER TABLE files_new RENAME TO files`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("移除 files.expires_at 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 files 表重建失败: %w", err)
	}

	// DROP TABLE 会连带删掉原表上的索引，索引必须重建。
	idxs := []string{
		`CREATE INDEX IF NOT EXISTS idx_files_kind ON files(kind, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_files_room ON files(room_code)`,
		`CREATE INDEX IF NOT EXISTS idx_files_sha ON files(sha256, kind, owner_id)`,
	}
	for _, q := range idxs {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("重建 files 索引失败: %w", err)
		}
	}
	return nil
}

// dropStoredNameUnique 若 files.stored_name 仍带 UNIQUE 约束，则重建该表去掉它。
//
// 判定方式：直接读 sqlite_master 里的建表语句找 "stored_name ... UNIQUE"。
// 不用 PRAGMA index_list —— 表级 UNIQUE 约束会显示成自动索引，
// 靠索引名（sqlite_autoindex_files_N）判断既脆弱又容易误判。
//
// 幂等：已经拆过的库直接返回。
func (s *Store) dropStoredNameUnique() error {
	var ddl string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='files'`).Scan(&ddl); err != nil {
		return fmt.Errorf("读取 files 表结构失败: %w", err)
	}
	if !needsDropUnique(ddl) {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 新表用与 schema 完全一致的列定义，只是 stored_name 不再 UNIQUE。
	stmts := []string{
		`CREATE TABLE files_new (
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
			pinned        INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO files_new (id, original_name, stored_name, size, sha256, kind,
		                        owner_id, owner_name, room_code, created_at, pinned)
		 SELECT id, original_name, stored_name, size, sha256, kind,
		        owner_id, owner_name, room_code, created_at, pinned FROM files`,
		`DROP TABLE files`,
		`ALTER TABLE files_new RENAME TO files`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("重建 files 表失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 files 表重建失败: %w", err)
	}

	// DROP TABLE 会连带删掉原表上的索引，索引必须重建。
	// idx_files_sha 由调用方在这之后重新创建（顺序见 migrate）。
	idxs := []string{
		`CREATE INDEX IF NOT EXISTS idx_files_kind ON files(kind, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_files_room ON files(room_code)`,
		`CREATE INDEX IF NOT EXISTS idx_files_sha ON files(sha256, kind, owner_id)`,
	}
	for _, q := range idxs {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("重建 files 索引失败: %w", err)
		}
	}
	return nil
}

// ensureFilePinnedColumn 保证 files 表有 pinned 列，缺则补。幂等。
//
// 单独抽出来是为了让 migrate 的调用顺序更好读：它被用在两个位置 ——
// 重建表之前（新库/普通老库的常规补列）与之后（重建路径的兜底），
// 两处都只是在回答「这一列到底有没有」。
func (s *Store) ensureFilePinnedColumn() error {
	has, err := s.columnExists("files", "pinned")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := s.db.Exec(
		`ALTER TABLE files ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("为文件表添加 pinned 列失败: %w", err)
	}
	return nil
}

// needsDropUnique 判断建表语句里 stored_name 列是否仍被声明为 UNIQUE。
//
// 只看 stored_name 那一行，避免误判其它列的 UNIQUE（比如未来给
// 别的列加唯一约束时不该触发这次重建）。
func needsDropUnique(ddl string) bool {
	for _, line := range strings.Split(ddl, "\n") {
		trimmed := strings.TrimSpace(strings.ToUpper(line))
		if !strings.HasPrefix(trimmed, "STORED_NAME") {
			continue
		}
		return strings.Contains(trimmed, "UNIQUE")
	}
	return false
}

// columnExists 判断某表是否有某列，用于增量迁移。
func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("读取表结构失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---------------------------------------------------------------- 配置

// 配置项的键。
const (
	// SettingRegistrationOpen 控制注册接口是否开放，值为 "1" 或 "0"。
	SettingRegistrationOpen = "registration_open"
)

// GetSetting 读取配置，不存在时返回空字符串。
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// SetSetting 写入配置（upsert）。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// IsRegistrationOpen 返回注册是否开放。
func (s *Store) IsRegistrationOpen() (bool, error) {
	// 空库必然开放：否则第一个管理员根本注册不进来，系统永远处于死局。
	if n, err := s.CountUsers(); err != nil {
		return false, err
	} else if n == 0 {
		return true, nil
	}
	v, err := s.GetSetting(SettingRegistrationOpen)
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// ---------------------------------------------------------------- 用户

// Role 是用户角色。第一版只有两种：
//
//	admin —— 可以删除任何人的文件，可以开关注册
//	user  —— 只能管理自己上传的文件
type Role string

const (
	// RoleAdmin 管理员。
	RoleAdmin Role = "admin"
	// RoleUser 普通用户。
	RoleUser Role = "user"
)

// User 是用户记录。
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         Role
	CreatedAt    time.Time
}

// IsAdmin 判断是否管理员。
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// CreateUser 新增用户。role 为空时按普通用户处理。
func (s *Store) CreateUser(username, passwordHash string, role Role) (*User, error) {
	username = strings.TrimSpace(username)
	if role == "" {
		role = RoleUser
	}
	now := time.Now().Unix()

	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?)`,
		username, passwordHash, string(role), now,
	)
	if err != nil {
		// 唯一索引冲突通常是重名，交给上层判断。
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{
		ID: id, Username: username, PasswordHash: passwordHash,
		Role: role, CreatedAt: time.Unix(now, 0),
	}, nil
}

// UserByName 按用户名查询。不存在返回 ErrNotFound。
func (s *Store) UserByName(username string) (*User, error) {
	row := s.db.QueryRow(
		`SELECT id, username, password_hash, role, created_at FROM users WHERE username = ?`,
		strings.TrimSpace(username),
	)
	var u User
	var role string
	var created int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &role, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Role = Role(role)
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

// UserByID 按 ID 查询。不存在返回 ErrNotFound。
func (s *Store) UserByID(id int64) (*User, error) {
	row := s.db.QueryRow(
		`SELECT id, username, password_hash, role, created_at FROM users WHERE id = ?`, id,
	)
	var u User
	var role string
	var created int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &role, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Role = Role(role)
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

// CountUsers 返回用户总数，用于判断是否需要初始化管理员。
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins 返回管理员数量，用于防止把最后一个管理员降级。
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, string(RoleAdmin)).Scan(&n)
	return n, err
}

// SetUserRole 修改用户角色。
func (s *Store) SetUserRole(id int64, role Role) error {
	_, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, string(role), id)
	return err
}

// ListUsers 列出全部用户（管理员界面用）。
func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`
		SELECT id, username, password_hash, role, created_at
		FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*User, 0, 8)
	for rows.Next() {
		var u User
		var role string
		var created int64
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &role, &created); err != nil {
			return nil, err
		}
		u.Role = Role(role)
		u.CreatedAt = time.Unix(created, 0)
		out = append(out, &u)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 会话

// Session 是服务端登录会话。
type Session struct {
	ID        string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession 写入一条会话记录。
func (s *Store) CreateSession(id string, userID int64, ttl time.Duration) error {
	now := time.Now()
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, userID, now.Unix(), now.Add(ttl).Unix(),
	)
	return err
}

// SessionUser 通过会话 ID 取用户；会话不存在或已过期返回 ErrNotFound。
func (s *Store) SessionUser(id string) (*User, error) {
	row := s.db.QueryRow(`
		SELECT u.id, u.username, u.password_hash, u.role, u.created_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ?`, id)

	var u User
	var role string
	var created, expires int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &role, &created, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if time.Now().Unix() > expires {
		_ = s.DeleteSession(id)
		return nil, ErrNotFound
	}
	u.Role = Role(role)
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

// DeleteSession 删除会话（登出）。
func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// CleanupSessions 删除过期会话，返回删除条数。
func (s *Store) CleanupSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- 文件

// FileKind 区分文件的归属与生命周期。
type FileKind string

const (
	// KindPermanent 是长期文件仓库中的文件，不会自动删除。
	//
	// 这是文件仓库唯一的类型。历史上还存在过 temporary（按时间过期），
	// 但 UI 早已只用 permanent，那套按 expires_at 回收的逻辑已成纯负债，
	// 因此被整体移除（连同 files.expires_at 列）。
	KindPermanent FileKind = "permanent"
	// KindChat 是随房间一起消亡的聊天室文件。
	//
	// 生命周期按**房间**存亡：房间被销毁（到点/空闲回收）时，
	// 其下所有文件立即删除，这样「房间到期即自动删除」这件事对文件也成立。
	KindChat FileKind = "chat"
)

// File 是文件元数据记录。
type File struct {
	ID           int64
	OriginalName string
	StoredName   string
	Size         int64
	SHA256       string
	Kind         FileKind
	OwnerID      *int64
	OwnerName    string
	// RoomCode 记录该文件属于哪个实时房间，仅 KindChat 有意义。
	// 房间销毁时按它找出所有关联文件一并删除。
	RoomCode  string
	CreatedAt time.Time
	// Pinned 表示该文件在仓库里被置顶。
	//
	// 只有永久文件会用到它：置顶解决的是「常用的那几个文件每次都要往下翻」，
	// 而聊天文件本身就是短命的，置顶没有意义（接口层也只能操作永久文件）。
	Pinned bool
}

// CreateFile 写入文件元数据。
func (s *Store) CreateFile(f *File) (int64, error) {
	var owner any
	if f.OwnerID != nil {
		owner = *f.OwnerID
	}

	res, err := s.db.Exec(`
		INSERT INTO files (original_name, stored_name, size, sha256, kind, owner_id, owner_name, room_code, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.OriginalName, f.StoredName, f.Size, f.SHA256, string(f.Kind),
		owner, f.OwnerName, f.RoomCode, f.CreatedAt.Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const fileCols = `id, original_name, stored_name, size, sha256, kind, owner_id, owner_name, room_code, created_at, pinned`

func scanFile(sc interface{ Scan(...any) error }) (*File, error) {
	var f File
	var kind string
	var owner sql.NullInt64
	var created int64
	var pinned int64

	if err := sc.Scan(&f.ID, &f.OriginalName, &f.StoredName, &f.Size, &f.SHA256,
		&kind, &owner, &f.OwnerName, &f.RoomCode, &created, &pinned); err != nil {
		return nil, err
	}
	f.Kind = FileKind(kind)
	if owner.Valid {
		v := owner.Int64
		f.OwnerID = &v
	}
	f.CreatedAt = time.Unix(created, 0)
	f.Pinned = pinned != 0
	return &f, nil
}

// ListFiles 按类型列出文件：置顶的最前，其余按上传时间从新到旧。
//
// 排序必须在服务端做：仓库是多人共用的共享盘，置顶是「这块盘上的共识」，
// 只有落在一个地方排序，所有客户端（含直连 API 的）看到的顺序才一致。
// 前端再排一次只会把这件事变成两处需要同步维护的逻辑。
func (s *Store) ListFiles(kind FileKind) ([]*File, error) {
	rows, err := s.db.Query(
		`SELECT `+fileCols+` FROM files WHERE kind = ?
		 ORDER BY pinned DESC, created_at DESC, id DESC`,
		string(kind),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*File, 0, 16)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FilesByRoom 列出某个实时房间下的全部聊天文件。
// 房间销毁时用它找出需要一并删除的文件。
func (s *Store) FilesByRoom(code string) ([]*File, error) {
	if code == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT `+fileCols+` FROM files WHERE kind = ? AND room_code = ?`,
		string(KindChat), code,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*File, 0, 8)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// OrphanChatFiles 返回「所属房间已不存在」的聊天文件。
//
// 兜底用途：进程在房间销毁与文件删除之间被杀掉时，
// 会留下一批再也无人引用的文件。清理协程用活跃房间号做差集把它们捞出来。
func (s *Store) OrphanChatFiles(activeCodes []string) ([]*File, error) {
	rows, err := s.db.Query(
		`SELECT `+fileCols+` FROM files WHERE kind = ?`, string(KindChat),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	active := make(map[string]struct{}, len(activeCodes))
	for _, c := range activeCodes {
		active[c] = struct{}{}
	}

	out := make([]*File, 0, 8)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		if _, ok := active[f.RoomCode]; !ok {
			out = append(out, f)
		}
	}
	return out, rows.Err()
}

// FileByID 按 ID 查询文件。
func (s *Store) FileByID(id int64) (*File, error) {
	row := s.db.QueryRow(`SELECT `+fileCols+` FROM files WHERE id = ?`, id)
	f, err := scanFile(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// FileBySHA256 按内容哈希查一条文件记录，用于文件仓库去重。
//
// 三个条件缺一不可：
//
//	sha256  内容相同才可能复用磁盘文件；
//	kind    刻意限定为 permanent —— 仓库文件是长期引用（多条记录可以
//	        指向同一个 stored_name），而临时/聊天文件是「谁先到期谁删磁盘」，
//	        一旦共享存储名就会互相误删。去重只对文件仓库开放。
//	owner   按上传者隔离 —— 磁盘文件是共享的，但记录是私有的。
//	        否则 A 可以靠「上传同一个文件」把 B 的记录顶掉/探出 B 的存在。
//
// 同一个人重复上传同一个文件时，这里会命中他自己最早的那条记录。
func (s *Store) FileBySHA256(sha string, kind FileKind, ownerID int64) (*File, error) {
	row := s.db.QueryRow(
		`SELECT `+fileCols+` FROM files
		 WHERE sha256 = ? AND kind = ? AND owner_id = ?
		 ORDER BY id ASC LIMIT 1`,
		sha, string(kind), ownerID,
	)
	f, err := scanFile(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// CountFilesBySHA256 统计某条内容哈希被多少条记录引用（限定类型与上传者）。
//
// 注意：删除路径**不再**用它决定要不要 unlink，改用 CountFilesByStoredName。
// 原因是它统计的是「同一上传者的同内容记录数」，而磁盘上的物理对象是
// stored_name；sha256 + kind + owner_id 这个组合想回答的其实是
// 「还有没有记录引用这个 stored_name」，多绕了一层反而可能漏判。
// 保留它是为了兼容去重查询的语义对照与既有测试。
func (s *Store) CountFilesBySHA256(sha string, kind FileKind, ownerID int64) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM files WHERE sha256 = ? AND kind = ? AND owner_id = ?`,
		sha, string(kind), ownerID,
	).Scan(&n)
	return n, err
}

// CountFilesByStoredName 统计还有多少条记录引用磁盘上的这个物理文件。
//
// 这是删除时唯一正确的引用计数口径：磁盘上被 unlink 的对象是 stored_name，
// 所以该问的问题就是「还有没有数据库记录指向它」。
//
// 不限定 kind / owner：即便某天出现跨类型共享存储名（当前设计上不会，
// 但历史数据或未来改动可能），只要还有一条记录指向它，就不能删。
// 宁可留下一个无人引用的孤儿文件（可由维护任务清理），
// 也不能删掉别人还在用的数据。
func (s *Store) CountFilesByStoredName(storedName string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM files WHERE stored_name = ?`,
		storedName,
	).Scan(&n)
	return n, err
}

// DeleteFile 删除文件记录，返回被删除的记录（用于同时删磁盘文件）。
func (s *Store) DeleteFile(id int64) (*File, error) {
	f, err := s.FileByID(id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM files WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return f, nil
}

// RenameFile 修改文件的展示名，返回改后的记录。
//
// 只管数据库里的 original_name —— 磁盘上的 stored_name 不动。
// 两者本来就是解耦的（存储名是随机生成的 <unixms>-<hex>.bin），
// 改展示名因此是一次纯元数据操作：不搬文件、不重算哈希，
// 对已经生成过的下载链接也没有任何影响。
//
// 还要特别注意「内容去重」：同一份内容可能被多条记录共享同一个 stored_name，
// 本方法只改 WHERE id = ? 命中的那一条，别人的文件名不受影响 ——
// 这正是「各人看到自己的文件名」得以成立的前提。
func (s *Store) RenameFile(id int64, name string) (*File, error) {
	if _, err := s.db.Exec(
		`UPDATE files SET original_name = ? WHERE id = ?`, name, id); err != nil {
		return nil, err
	}
	// 回读而不是拼一个：调用方拿到的是数据库里真实生效的样子，
	// 也顺带把「id 不存在」这件事变成 ErrNotFound 暴露出去。
	return s.FileByID(id)
}

// SetFilePinned 设置文件的置顶状态，返回改后的记录。
func (s *Store) SetFilePinned(id int64, pinned bool) (*File, error) {
	// SQLite 没有布尔字面量，存 0/1。
	v := 0
	if pinned {
		v = 1
	}
	if _, err := s.db.Exec(
		`UPDATE files SET pinned = ? WHERE id = ?`, v, id); err != nil {
		return nil, err
	}
	return s.FileByID(id)
}

// DeleteFileRecord 只删记录，不碰磁盘（清理时磁盘由调用方删）。
func (s *Store) DeleteFileRecord(id int64) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE id = ?`, id)
	return err
}

// TotalUsage 返回某一类型的文件总占用字节数。
func (s *Store) TotalUsage(kind FileKind) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT SUM(size) FROM files WHERE kind = ?`, string(kind)).Scan(&n)
	return n.Int64, err
}
