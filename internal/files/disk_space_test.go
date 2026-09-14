//go:build !windows

package files

import (
	"errors"
	"testing"
)

// 要一个任何分区都放不下的容量：1 EB。
const impossibleSize int64 = 1 << 60

func TestHasRoomForUploadRejectsImpossibleSize(t *testing.T) {
	svc, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("创建文件服务失败: %v", err)
	}

	err = svc.HasRoomForUpload(impossibleSize)
	if !errors.Is(err, ErrNoSpace) {
		t.Errorf("HasRoomForUpload(%d) = %v, 期望 ErrNoSpace", impossibleSize, err)
	}
}

func TestHasRoomForUploadAllowsReasonableSize(t *testing.T) {
	svc, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("创建文件服务失败: %v", err)
	}

	// 1 KB 在任何还能跑测试的机器上都应该放得下。
	if err := svc.HasRoomForUpload(1024); err != nil {
		t.Errorf("HasRoomForUpload(1024) = %v, 期望 nil", err)
	}
}
