package files

import "testing"

// 长度未知（chunked 上传）时不做判断 —— 猜错了要么误拒要么漏检，
// 不如放过，让真正的写入去失败。
func TestHasRoomForUploadSkipsWhenSizeUnknown(t *testing.T) {
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("创建文件服务失败: %v", err)
	}

	for _, size := range []int64{0, -1} {
		if err := svc.HasRoomForUpload(size); err != nil {
			t.Errorf("HasRoomForUpload(%d) = %v, 期望跳过检查返回 nil", size, err)
		}
	}
}
