package api

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// buildMultipart 按给定顺序拼一个 multipart 请求体。
//
// 顺序是参数而不是固定值：正因为服务端是流式解析，
// 「字段放在文件前面还是后面」会真实影响它能读到什么，必须能测。
func buildMultipart(t *testing.T, parts []partSpec) (contentType string, body *bytes.Buffer) {
	t.Helper()

	body = &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for _, p := range parts {
		if p.filename != "" {
			fw, err := mw.CreateFormFile(p.field, p.filename)
			if err != nil {
				t.Fatalf("创建文件字段失败: %v", err)
			}
			if _, err := fw.Write([]byte(p.content)); err != nil {
				t.Fatalf("写入文件内容失败: %v", err)
			}
			continue
		}
		if err := mw.WriteField(p.field, p.content); err != nil {
			t.Fatalf("写入文本字段失败: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("结束 multipart 失败: %v", err)
	}
	return mw.FormDataContentType(), body
}

type partSpec struct {
	field    string
	filename string // 非空表示这是文件字段
	content  string
}

func newUploadRequest(t *testing.T, parts []partSpec) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()

	ct, body := buildMultipart(t, parts)
	r := httptest.NewRequest(http.MethodPost, "/api/files", body)
	r.Header.Set("Content-Type", ct)
	return httptest.NewRecorder(), r
}

func TestOpenUploadStreamFieldsBeforeFile(t *testing.T) {
	w, r := newUploadRequest(t, []partSpec{
		{field: "kind", content: "permanent"},
		{field: "name", content: "报告.pdf"},
		{field: "file", filename: "raw-name.bin", content: "hello world"},
	})

	up, err := openUploadStream(w, r, "file", 0)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	defer up.Close()

	if got := up.Field("kind"); got != "permanent" {
		t.Errorf("kind = %q, 期望 permanent", got)
	}
	if got := up.Field("name"); got != "报告.pdf" {
		t.Errorf("name = %q, 期望 报告.pdf", got)
	}
	if up.Filename != "raw-name.bin" {
		t.Errorf("Filename = %q, 期望 raw-name.bin", up.Filename)
	}

	data, err := io.ReadAll(up.Body)
	if err != nil {
		t.Fatalf("读取文件流失败: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("文件内容 = %q, 期望 hello world", data)
	}
}

// 流式解析无法回头读已经跳过的 part，所以文件排在字段前面时 kind 必然拿不到。
// 这条测试把这个行为钉住 —— handleUpload 正是靠它按最严格的权限兜底。
func TestOpenUploadStreamFieldsAfterFileAreInvisible(t *testing.T) {
	w, r := newUploadRequest(t, []partSpec{
		{field: "file", filename: "a.bin", content: "x"},
		{field: "kind", content: "permanent"},
	})

	up, err := openUploadStream(w, r, "file", 0)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	defer up.Close()

	if got := up.Field("kind"); got != "" {
		t.Errorf("kind = %q, 期望空串（文件之后的字段读不到）", got)
	}
}

func TestOpenUploadStreamNoFilePart(t *testing.T) {
	w, r := newUploadRequest(t, []partSpec{
		{field: "kind", content: "permanent"},
	})

	if _, err := openUploadStream(w, r, "file", 0); !errors.Is(err, ErrNoFilePart) {
		t.Errorf("err = %v, 期望 ErrNoFilePart", err)
	}
}

// 超限不是在读 part 头时暴露，而是延后到真正读超出那一字节 ——
// 也就是落盘阶段。这里验证两处都能认出来，否则会返回 500 而不是 413。
func TestOpenUploadStreamTooLarge(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 1<<20+4096) // 超过 1MB 的 multipart 开销补偿
	w, r := newUploadRequest(t, []partSpec{
		{field: "file", filename: "big.bin", content: string(big)},
	})

	up, err := openUploadStream(w, r, "file", 8)
	if err != nil {
		if !errors.Is(err, ErrUploadTooLarge) {
			t.Fatalf("err = %v, 期望 ErrUploadTooLarge", err)
		}
		return // 在读 part 阶段就超限了，同样合理
	}
	defer up.Close()

	if _, err := io.ReadAll(up.Body); !isRequestBodyTooLarge(err) {
		t.Errorf("读取超限内容 err = %v, 期望被识别为请求体过大", err)
	}
}

func TestOpenUploadStreamNotMultipart(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString("plain"))
	r.Header.Set("Content-Type", "text/plain")

	if _, err := openUploadStream(httptest.NewRecorder(), r, "file", 0); err == nil {
		t.Fatal("非 multipart 请求应当报错")
	}
}
