//go:build windows

package files

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	modKernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = modKernel32.NewProc("GetDiskFreeSpaceExW")
)

// diskSpace 返回 dir 所在分区的可用字节数与总字节数。
//
// 通过 Windows API GetDiskFreeSpaceExW 查询当前用户可用配额与卷总容量。
func diskSpace(dir string) (free, total uint64, err error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}

	var freeBytesAvail, totalBytes, totalFree uint64
	r1, _, e1 := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeBytesAvail)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		if e1 != nil && e1 != syscall.Errno(0) {
			return 0, 0, e1
		}
		return 0, 0, errors.New("获取磁盘空间失败")
	}
	return freeBytesAvail, totalBytes, nil
}
