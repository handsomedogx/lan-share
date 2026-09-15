package files

import (
	"path"
	"strings"
)

// 图片类型：聊天里的图片要**内联显示**，浏览器必须拿到正确的 Content-Type
// 且不能被当成下载附件。而这两件事都取决于「这是不是图片、是哪种图片」，
// 所以判定必须由服务端给出唯一答案 —— 让每个客户端各自猜扩展名，
// 迟早会出现「甲看到缩略图、乙看到下载卡片」这种不一致。
//
// 白名单而不是黑名单：只放行浏览器能安全直接渲染的位图格式。
// svg 被**刻意排除** —— 它是 XML，可以内嵌 <script>。即使加了 nosniff，
// 只要 Content-Type 是 image/svg+xml，直接导航到该 URL 仍会执行脚本
// （nosniff 拦的是「类型嗅探」，不是同类型内的脚本）。
// 而聊天文件是免登录可访问的，等于给任何进过房间的人发了一个
// 同源脚本执行入口。代价只是「SVG 不显示缩略图」，很划算。
var imageMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".jfif": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".avif": "image/avif",
	".ico":  "image/x-icon",
}

// IconExts 是上面白名单对应的扩展名集合，测试用它对齐两处清单。
func IconExts() []string {
	out := make([]string, 0, len(imageMIME))
	for ext := range imageMIME {
		out = append(out, ext)
	}
	return out
}

// ImageExt 返回文件名的规范化扩展名（含点，小写）；不是已知图片则返回空串。
//
// 只看扩展名，不读文件头魔数：聊天上传是流式的，文件在落盘前就开始转发，
// 为了判类型把开头几个字节缓冲下来会牵动整条上传链路；而扩展名对
// 「要不要显示成图片」这个用途已经够用，猜错时前端有 onerror 兜底。
//
// 传进来的必须是**已经过 SafeDisplayName 清洗**的名字 —— 这里不再做防御，
// 但仍只用 path.Ext 取最后一段，天然不会受路径影响。
func ImageExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if _, ok := imageMIME[ext]; ok {
		return ext
	}
	return ""
}

// ImageMIME 返回该扩展名对应的 Content-Type；非图片返回空串。
func ImageMIME(ext string) string {
	return imageMIME[strings.ToLower(ext)]
}

// IsImageName 判断文件名是否是白名单内的图片。
func IsImageName(name string) bool { return ImageExt(name) != "" }
