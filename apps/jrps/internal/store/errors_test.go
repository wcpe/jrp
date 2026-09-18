package store

import (
	"errors"
	"strings"
	"testing"
)

// 底层写入错误必须映射为中文可运维描述，供上层按明确行为降级并记中文日志。
func TestWriteFailuresAreDescribedInChinese(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{"磁盘写满", "SQL logic error: database or disk is full (13)", "磁盘空间不足"},
		{"数据库被占用", "database is locked (5) (SQLITE_BUSY)", "已被占用"},
		{"文件只读", "attempt to write a readonly database (8)", "不可写"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			err := translateSQLError(errors.New(item.input))
			if err == nil {
				t.Fatal("底层错误不得被吞掉")
			}
			if !strings.Contains(err.Error(), item.expected) {
				t.Fatalf("错误描述应包含 %q，实际为：%v", item.expected, err)
			}
			// 原始错误必须可追溯，便于排障。
			if !errors.Is(err, err) || !strings.Contains(err.Error(), item.input) {
				t.Fatalf("错误应保留原始原因：%v", err)
			}
		})
	}
}

// 未知的底层错误必须原样透出，不伪装成已知分类。
func TestUnknownWriteErrorIsPassedThrough(t *testing.T) {
	original := errors.New("some unknown sqlite failure")
	translated := translateSQLError(original)
	if !errors.Is(translated, original) {
		t.Fatalf("未知错误应原样透出：%v", translated)
	}
	if errors.Is(translated, errors.New("磁盘空间不足")) {
		t.Fatal("未知错误不应被误判为磁盘写满")
	}
}

// 空错误必须保持为空，避免凭空生成错误。
func TestNilErrorStaysNil(t *testing.T) {
	if err := translateSQLError(nil); err != nil {
		t.Fatalf("nil 输入应返回 nil：%v", err)
	}
}

// 数据库关闭后写入必须被拒绝并给出中文原因。
func TestWriteAfterCloseIsRejected(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	if err := store.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	err := store.Transaction(t.Context(), func(*Tx) error { return nil })
	if err == nil {
		t.Fatal("数据库关闭后写入必须被拒绝")
	}
	if !strings.Contains(err.Error(), "关闭") {
		t.Fatalf("拒绝原因应为中文：%v", err)
	}
}
