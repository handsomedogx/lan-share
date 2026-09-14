//go:build windows

package files

import "errors"

// diskSpace 在 Windows 上不实现。
//
// 这个程序只跑在 Linux 路由器上，Windows 仅仅用于本地开发调试。
// 这里返回错误，调用方会「跳过空间检查」而不是「拒绝上传」——
// 为了一个只在生产环境才用得上的检查去引入平台依赖不值得。
func diskSpace(dir string) (free, total uint64, err error) {
	return 0, 0, errors.New("当前平台不支持查询磁盘空间")
}
