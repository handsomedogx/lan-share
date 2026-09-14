// Package auth 负责用户密码哈希与登录。
//
// 密码一律使用 bcrypt 哈希后存储，绝不保存明文。
// 选择 bcrypt 而不是 Argon2id 的原因：golang.org/x/crypto/bcrypt 是
// 纯 Go 实现、零 CGO 依赖、API 简单，且自带 salt，适合路由器环境。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"lanshare/internal/storage"
)

// bcryptCost 定为 10：路由器 CPU 较弱，10 在安全与耗时之间比较平衡
// （单次哈希约数十毫秒，登录场景完全够用，又不会拖慢 Radxa/AX1800 这类设备）。
const bcryptCost = 10

var (
	// ErrInvalidCredentials 表示用户名或密码错误。
	// 对外统一返回这个错误，不区分「用户不存在」与「密码错误」，避免账号枚举。
	ErrInvalidCredentials = errors.New("用户名或密码错误")

	// ErrWeakPassword 表示密码不符合最低强度要求。
	ErrWeakPassword = errors.New("密码长度至少 6 位")

	// ErrEmptyUsername 表示用户名为空。
	ErrEmptyUsername = errors.New("用户名不能为空")

	// ErrUserExists 表示用户名已被占用。
	ErrUserExists = errors.New("该用户名已被使用")
)

// Service 提供登录相关能力。
type Service struct {
	store *storage.Store
}

// New 创建认证服务。
func New(store *storage.Store) *Service { return &Service{store: store} }

// Hash 生成密码哈希。
func Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("生成密码哈希失败: %w", err)
	}
	return string(b), nil
}

// CheckPassword 校验明文密码与哈希是否匹配。
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ValidateCredentials 做基本的输入合法性检查。
func ValidateCredentials(username, password string) error {
	if strings.TrimSpace(username) == "" {
		return ErrEmptyUsername
	}
	if len([]rune(password)) < 6 {
		return ErrWeakPassword
	}
	// 用户名只允许字母数字与下划线、中划线，避免奇怪的路径/日志注入。
	for _, r := range username {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return errors.New("用户名只能包含字母、数字、下划线和短横线")
		}
	}
	return nil
}

// FirstUserBecomesAdmin 说明注册策略：
// 系统里还没有任何用户时，第一个注册的人自动成为管理员，此后注册的都是普通用户。
// 这样首次部署可以完全在浏览器里完成，不需要 SSH 上路由器建号。
//
// 同时提供两道闸门防止「谁抢到谁是管理员」：
//  1. 注册默认只在「还没有用户」时开放；有用户后自动关闭，需管理员手动开启；
//  2. 路由器上服务只监听回环，由 nginx 反代，外部网络本就碰不到。
const FirstUserBecomesAdmin = true

// Register 创建用户。
//
// 首个用户自动授予管理员角色。用户名重复时返回 ErrUserExists，
// 由上层翻译成友好提示（之前这里直接漏了底层 SQLite 错误给用户看）。
func (s *Service) Register(username, password string) (*storage.User, error) {
	if err := ValidateCredentials(username, password); err != nil {
		return nil, err
	}

	n, err := s.store.CountUsers()
	if err != nil {
		return nil, fmt.Errorf("统计用户数失败: %w", err)
	}

	role := storage.RoleUser
	if n == 0 {
		role = storage.RoleAdmin
	}

	hash, err := Hash(password)
	if err != nil {
		return nil, err
	}
	u, err := s.store.CreateUser(strings.TrimSpace(username), hash, role)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrUserExists
		}
		return nil, err
	}
	return u, nil
}

// isUniqueViolation 粗略识别唯一约束冲突。
// modernc 驱动返回的文本里含 "UNIQUE constraint failed"。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}

// Login 校验凭据，成功返回用户。
func (s *Service) Login(username, password string) (*storage.User, error) {
	u, err := s.store.UserByName(username)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// 做一次等价耗时的哈希，避免通过响应时间探测用户是否存在。
			_ = CheckPassword("$2a$10$0000000000000000000000000000000000000000000000000000", password)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}
