//go:build !windows

package files

import "syscall"

// diskSpace 返回 dir 所在分区的可用字节数与总字节数。
//
// 用 statfs 而不是「写个文件试试」：前者是一次系统调用，
// 后者会在磁盘本来就满的时候制造一次真实的写入失败，纯属添乱。
func diskSpace(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}

	// Bavail 而不是 Bfree：Bfree 把 root 预留的块也算成可用，
	// 普通进程其实写不进去，用它会高估可用空间。
	return uint64(st.Bavail) * uint64(st.Bsize),
		uint64(st.Blocks) * uint64(st.Bsize),
		nil
}
