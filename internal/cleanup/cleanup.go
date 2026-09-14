// Package cleanup 周期性清理临时文件、无主聊天文件与过期登录会话。
//
// 路由器上跑的定时任务必须足够轻：这里每 10 分钟醒一次，
// 每次只做几条 SQL 与若干次 unlink，几乎不占 CPU。
package cleanup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lanshare/internal/files"
	"lanshare/internal/logger"
	"lanshare/internal/session"
	"lanshare/internal/storage"
)

// interval 是清理周期。
const interval = 10 * time.Minute

// staleUploadAge 是残留文件被视为「上一轮没传完」的时限。
//
// 24 小时对一次上传来说宽得离谱 —— 这正是它安全的原因：
// 正在写的文件 mtime 会一直更新，不会被误判成残留。
const staleUploadAge = 24 * time.Hour

// Worker 是清理任务。
type Worker struct {
	store *storage.Store
	files *files.Service
	// sessions 用来取当前活跃房间号，从而识别聊天文件是否已成孤儿。
	// 允许为 nil（单元测试或不需要该能力的场景）。
	sessions *session.Manager
	log      *logger.Logger

	// tmpDir 是 TMPDIR 指向的目录，用于回收历史临时文件。
	// 留空表示跳过这一步。
	tmpDir string
}

// SetTmpDir 指定临时目录。
//
// 单独给个 setter 而不是塞进 New 的参数列表：它是由部署环境决定的可选项，
// 本地开发时不设就自动跳过，没必要为此改构造签名。
func (w *Worker) SetTmpDir(dir string) { w.tmpDir = dir }

// New 创建清理任务。
func New(store *storage.Store, fs *files.Service, sessions *session.Manager, log *logger.Logger) *Worker {
	return &Worker{store: store, files: fs, sessions: sessions, log: log}
}

// Run 阻塞运行清理循环，直到 ctx 取消。
func (w *Worker) Run(ctx context.Context) {
	// 启动后先跑一次，覆盖「上次关机时留下的过期文件」。
	w.tick()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick()
		}
	}
}

func (w *Worker) tick() {
	// 1. 过期临时文件：先删磁盘，再删记录。
	//    顺序很重要 —— 万一删磁盘失败，记录还在，下轮会重试；
	//    反之则会出现「有记录没文件」的死数据。
	expired, err := w.store.ExpiredFiles()
	if err != nil {
		w.log.Error("查询过期文件失败: %v", err)
	} else {
		for _, f := range expired {
			if err := w.files.Remove(string(f.Kind), f.StoredName); err != nil {
				w.log.Error("删除过期文件 %s 失败: %v", f.StoredName, err)
				continue
			}
			if err := w.store.DeleteFileRecord(f.ID); err != nil {
				w.log.Error("删除过期文件记录 %d 失败: %v", f.ID, err)
				continue
			}
			w.log.Info("已清理过期临时文件: %s (%d 字节)", f.OriginalName, f.Size)
		}
	}

	// 2. 无主聊天文件。
	//
	// 正常路径下房间销毁会立刻删掉自己名下的文件（session.Manager 的回调）。
	// 但若进程在「房间已删、文件未删」之间被杀掉，就会留下再也无人引用的文件。
	// 这里用「活跃房间号」做差集兜底，保证磁盘不会缓慢泄漏。
	if w.sessions != nil {
		orphans, err := w.store.OrphanChatFiles(w.sessions.ActiveCodes())
		if err != nil {
			w.log.Error("查询无主聊天文件失败: %v", err)
		} else {
			for _, f := range orphans {
				if err := w.files.Remove(string(f.Kind), f.StoredName); err != nil {
					w.log.Error("删除无主聊天文件 %s 失败: %v", f.StoredName, err)
					continue
				}
				if err := w.store.DeleteFileRecord(f.ID); err != nil {
					w.log.Error("删除无主聊天文件记录 %d 失败: %v", f.ID, err)
					continue
				}
				w.log.Info("已清理无主聊天文件: %s (房间 %s, %d 字节)",
					f.OriginalName, f.RoomCode, f.Size)
			}
		}
	}

	// 4. 残留的 .part（半成品文件）。
	//
	// 正常路径下它们写完就 rename、失败就当场删；留到这里的都是
	// 进程被杀、路由器掉电这类意外留下的，会一直占着磁盘没人认领。
	if n, freed, err := w.files.PurgeStaleParts(); err != nil {
		w.log.Error("清理残留未完成文件失败: %v", err)
	} else if n > 0 {
		w.log.Info("已清理残留未完成文件 %d 个（%s）", n, files.HumanSize(freed))
	}

	// 5. 临时目录里的历史文件。
	w.purgeStaleTemp()

	// 6. 过期登录会话。
	if n, err := w.store.CleanupSessions(); err != nil {
		w.log.Error("清理过期会话失败: %v", err)
	} else if n > 0 {
		w.log.Info("已清理过期登录会话 %d 条", n)
	}
}

// purgeStaleTemp 清理 TMPDIR 里我们自己留下的历史临时文件。
//
// 两个刻意的保守：
//   - 只认三种名字（multipart-* / upload-* / *.part）；
//   - 只用 os.Remove 逐个删，绝不用 os.RemoveAll 清整个目录 ——
//     那样会在服务重启时把别的进程、乃至正在进行的上传的临时文件一起端掉。
func (w *Worker) purgeStaleTemp() {
	if w.tmpDir == "" {
		return
	}

	entries, err := os.ReadDir(w.tmpDir)
	if err != nil {
		if !os.IsNotExist(err) {
			w.log.Error("读取临时目录失败: %v", err)
		}
		return
	}

	cutoff := time.Now().Add(-staleUploadAge)
	var n int
	var freed int64
	for _, e := range entries {
		if e.IsDir() || !isOurTempFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(w.tmpDir, e.Name())); err != nil {
			continue
		}
		n++
		freed += info.Size()
	}
	if n > 0 {
		w.log.Info("已清理临时目录残留文件 %d 个（%s）", n, files.HumanSize(freed))
	}
}

// isOurTempFile 判断一个临时文件名是不是 LAN Share 自己的产物。
func isOurTempFile(name string) bool {
	return strings.HasPrefix(name, "multipart-") ||
		strings.HasPrefix(name, "upload-") ||
		strings.HasSuffix(name, ".part")
}
