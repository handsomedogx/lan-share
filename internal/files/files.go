// Package files 负责文件仓库的落盘、读取与删除。
//
// 关键设计（对应文档第 8 节）：
//   - 文件本体一律存文件系统，绝不写入 SQLite；
//   - SQLite 只保存元数据（ID / 原始文件名 / 存储名 / 大小 / SHA256 / 上传人 / 时间）；
//   - 存储名使用「时间戳 + 随机串」生成，与原始文件名解耦，
//     这样即使用户上传 ../../../etc/passwd 这类文件名也不可能穿越目录。
package files

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrTooLarge 表示上传超过限制。
var ErrTooLarge = errors.New("文件超过大小限制")

// ErrNoSpace 表示目标分区放不下这次上传。
var ErrNoSpace = errors.New("磁盘空间不足")

// reservedSpaceRatio 是留给系统和其他服务的空间比例。
//
// 把磁盘写到 100% 满不是「刚好用完」那么简单：日志、数据库、以及路由器上
// 同时跑的其它服务都会跟着一起挂。留 5% 能让它们在磁盘紧张时还能喘口气。
const reservedSpaceRatio = 0.05

// Service 提供文件落盘能力。
type Service struct {
	root     string // 文件仓库根目录
	maxBytes int64  // 单文件上限，0 表示不限制
}

// New 创建文件服务，并确保各分类子目录存在。
func New(root string, maxBytes int64) (*Service, error) {
	for _, sub := range []string{"permanent", "temporary", "chat"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("创建文件目录失败: %w", err)
		}
	}
	return &Service{root: root, maxBytes: maxBytes}, nil
}

// Dir 返回某类型的存放目录。
//
// 用白名单而不是直接拼用户输入：kind 会进到路径里，
// 虽然调用方目前只传 storage 包里的常量，但这里不留下任何拼接隐患。
func (s *Service) Dir(kind string) string {
	switch kind {
	case "temporary":
		return filepath.Join(s.root, "temporary")
	case "chat":
		return filepath.Join(s.root, "chat")
	default:
		return filepath.Join(s.root, "permanent")
	}
}

// SaveResult 是一次保存的结果。
type SaveResult struct {
	StoredName string
	Size       int64
	SHA256     string
}

// Save 把 r 的内容写入指定类型的目录，同时计算大小与 SHA256。
//
// 采用「先写 .part 临时文件，成功后 rename」的方式：
// 这样即使写到一半进程被杀，也不会在仓库里留下一个看似完整、实际损坏的文件。
func (s *Service) Save(kind string, r io.Reader) (*SaveResult, error) {
	dir := s.Dir(kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	stored, err := newStoredName()
	if err != nil {
		return nil, err
	}

	finalPath := filepath.Join(dir, stored)
	partPath := finalPath + ".part"

	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}

	// 出错路径统一清理 .part。
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(partPath)
		}
	}()

	hasher := sha256.New()
	var reader io.Reader = r

	if s.maxBytes > 0 {
		// 多读 1 字节用于判断是否超限。
		reader = io.LimitReader(r, s.maxBytes+1)
	}

	// io.Copy 出错时返回的 n 是「中断前已写入多少」，这是区分故障类型的
	// 关键线索：n 远小于文件说明网络断了，n 接近上限则可能是磁盘满了。
	n, err := io.Copy(io.MultiWriter(f, hasher), reader)
	if err != nil {
		return nil, fmt.Errorf("写入文件失败（已接收 %s）: %w", HumanSize(n), err)
	}
	if s.maxBytes > 0 && n > s.maxBytes {
		err = ErrTooLarge
		return nil, err
	}

	if err = f.Sync(); err != nil {
		return nil, fmt.Errorf("刷盘失败: %w", err)
	}
	if err = f.Close(); err != nil {
		return nil, fmt.Errorf("关闭文件失败: %w", err)
	}
	if err = os.Rename(partPath, finalPath); err != nil {
		return nil, fmt.Errorf("重命名文件失败: %w", err)
	}

	return &SaveResult{
		StoredName: stored,
		Size:       n,
		SHA256:     hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// HasRoomForUpload 判断文件仓库所在分区是否还放得下 size 字节。
//
// 探的是仓库根目录：调用方在真正落盘前往往还不知道文件类型
// （kind 还在没解析的 multipart 里），而三类目录都在根目录下，
// 直接探根目录既准确，也不用假设它们一定在同一个分区。
//
// 为什么要提前问一次：不查的话，一个 1GB 文件会一直传到 40% 才因为磁盘满
// 而失败，用户白等半天，服务端还白写了一次 .part。提前拒掉只要一次 statfs。
//
// 两处刻意的「放行」：
//   - size <= 0（chunked 上传，长度未知）→ 不猜，让真正的写入去失败；
//   - statfs 本身出错 → 当作空间足够。宁可让上传在写盘时失败，
//     也不要因为一次系统调用失败就把所有上传挡在门外。
func (s *Service) HasRoomForUpload(size int64) error {
	if size <= 0 {
		return nil
	}

	free, total, err := diskSpace(s.root)
	if err != nil {
		return nil
	}

	reserve := uint64(float64(total) * reservedSpaceRatio)
	need := uint64(size) + reserve
	if need > free {
		return fmt.Errorf("%w: 需要 %s，可用 %s（其中 %s 为系统预留）",
			ErrNoSpace, HumanSize(int64(need)), HumanSize(int64(free)), HumanSize(int64(reserve)))
	}
	return nil
}

// Open 以只读方式打开一个已存储的文件。
func (s *Service) Open(kind, storedName string) (*os.File, error) {
	// 二次防御：存储名只可能是我们自己生成的安全字符集。
	if !isSafeName(storedName) {
		return nil, errors.New("非法的存储文件名")
	}
	return os.Open(filepath.Join(s.Dir(kind), storedName))
}

// Remove 删除一个已存储的文件。文件不存在视为成功（幂等）。
func (s *Service) Remove(kind, storedName string) error {
	if !isSafeName(storedName) {
		return errors.New("非法的存储文件名")
	}
	err := os.Remove(filepath.Join(s.Dir(kind), storedName))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// newStoredName 生成存储名：<unix毫秒>-<8字节随机hex>.bin
//
// 使用 .bin 后缀是刻意的 —— 存储名完全不暴露原始类型，
// 下载时一律通过 Content-Disposition 还原原始文件名，
// 也从侧面降低了「上传 html 后被当页面执行」的风险。
func newStoredName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机文件名失败: %w", err)
	}
	return fmt.Sprintf("%d-%s.bin", time.Now().UnixMilli(), hex.EncodeToString(b)), nil
}

// isSafeName 校验存储名只包含安全字符，杜绝路径穿越。
func isSafeName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) || strings.Contains(name, "..") {
		return false
	}
	return true
}

// SafeDisplayName 清洗用户提供的文件名，用于展示与下载头。
//
// 去掉路径部分与危险字符，保留中文；空名回退为 "file"。
func SafeDisplayName(name string) string {
	name = strings.TrimSpace(name)
	// 客户端可能传完整路径（例如旧浏览器），只取最后一段。
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case 0, '\r', '\n', '"':
			return -1
		}
		if r < 32 {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "file"
	}
	if len([]rune(name)) > 200 {
		r := []rune(name)
		name = string(r[:200])
	}
	return name
}

// HumanSize 把字节数格式化成便于阅读的字符串。
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
