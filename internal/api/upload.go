package api

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// 本文件是上传入口的公共实现，文件仓库与聊天室两条链路共用同一套流式解析，
// 避免两边各写一份、各错各的。
//
// 为什么不能用 r.ParseMultipartForm：
//
//	它会把超过内存阈值的部分写进 os.CreateTemp 的临时文件。在路由器上
//	（TMPDIR 未改时）那就是 tmpfs = 内存，于是「传一个 125MB 文件」≈
//	「吃掉 125MB RAM」，本项目曾因此被内核 OOM Killer 杀掉。
//	即便 TMPDIR 已指到数据分区，它仍然意味着：文件先在磁盘上完整写一遍，
//	再被 io.Copy 复制到正式文件 —— 一次上传两遍写、一遍读，
//	在 eMMC/SD 上纯属浪费，还带来「进度到 100% 后还要再等一轮」的停顿。
//
// 改用 r.MultipartReader() 后，文件 part 本身就是 io.Reader，
// 直接交给存储层：数据只经过 io.Copy 的一块小缓冲就落盘，
// 内存占用与文件大小无关，磁盘也只写一遍。

// ErrUploadTooLarge 表示请求体超过允许的上限。
var ErrUploadTooLarge = errors.New("上传内容超过大小限制")

// ErrNoFilePart 表示 multipart 里没有找到文件字段。
var ErrNoFilePart = errors.New("没有收到文件")

const (
	// maxFormFieldBytes 是单个文本字段的读取上限。
	//
	// 只需要装下 kind 和一个文件名，4KB 绰绰有余；
	// 关键是必须有上限 —— 否则一个永不结束的文本字段就能把内存吃干。
	maxFormFieldBytes = 4 << 10

	// multipartOverhead 是 multipart 封装自身的额外开销（boundary、part 头）。
	//
	// MaxBytesReader 限的是整个请求体，而它比文件内容多出这些字节；
	// 不补偿的话「刚好等于上限的文件」会被误判为超限。
	multipartOverhead = 1 << 20
)

// uploadStream 是一次流式打开的上传。
type uploadStream struct {
	// Fields 只包含「出现在文件 part 之前」的文本字段。
	//
	// 流式解析没法回头读已经跳过的 part，所以前端约定
	// 把 kind / name 放在 file 之前（见 web/js/app.js 的 uploadOne）。
	//
	// 若文件 part 先到，Fields 里就没有 kind，调用方会按最严格的方式处理
	// （见 handleUpload），不会因此在权限未明的情况下放行。
	Fields map[string]string

	// Filename 是客户端在 part 头里给的文件名，未做清洗，仅供回退取名。
	Filename string

	// Body 是文件内容流，只能读一次。
	Body io.Reader

	part *multipart.Part
}

// Field 取一个已读到的文本字段，取不到返回空串。
func (u *uploadStream) Field(name string) string {
	if u == nil || u.Fields == nil {
		return ""
	}
	return strings.TrimSpace(u.Fields[name])
}

// Close 关闭文件 part。
//
// 无论成功失败都要调用：不关闭的话底层连接读不完，
// 会让本可复用的连接被服务端直接关掉。
func (u *uploadStream) Close() {
	if u != nil && u.part != nil {
		_ = u.part.Close()
	}
}

// openUploadStream 流式打开 multipart 请求里的文件字段。
//
// 它在遇到文件 part 的那一刻就返回，后面的 part 不再读取 ——
// 这正是「边收边写」所需要的：调用方拿到的 Body 背后仍是网络连接，
// 读完即落盘，中间不经过任何临时文件。
//
// maxBytes 为 0 表示不限制（与 files.Service 的约定一致）。
func openUploadStream(w http.ResponseWriter, r *http.Request, fileField string, maxBytes int64) (*uploadStream, error) {
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+multipartOverhead)
	}

	mr, err := r.MultipartReader()
	if err != nil {
		return nil, errors.New("不是合法的 multipart 请求")
	}

	u := &uploadStream{Fields: make(map[string]string)}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, translateUploadErr(err)
		}

		name := part.FormName()

		if name == fileField {
			// 文件 part 直接交出去，既不读进内存也不落临时文件。
			u.Filename = part.FileName()
			u.part = part
			u.Body = part
			return u, nil
		}

		// 文本字段：限量读进内存即可。
		b, readErr := io.ReadAll(io.LimitReader(part, maxFormFieldBytes))
		_ = part.Close()
		if readErr != nil {
			return nil, translateUploadErr(readErr)
		}
		u.Fields[name] = string(b)
	}

	return nil, ErrNoFilePart
}

// translateUploadErr 把底层错误翻译成上层能判断的语义。
func translateUploadErr(err error) error {
	if isRequestBodyTooLarge(err) {
		return ErrUploadTooLarge
	}
	// 其余（连接中断、非法 boundary 等）原样上抛，由上层当普通失败处理。
	return err
}

// isRequestBodyTooLarge 判断错误是否来自请求体超限。
//
// 流式上传时 MaxBytesReader 不会在读 part 头时就报错，
// 而是延后到真正读到超出那一字节 —— 也就是落盘阶段才暴露。
// 所以「打开流」和「写文件」两处都要认一次，否则会报成 500 而不是 413。
func isRequestBodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}
