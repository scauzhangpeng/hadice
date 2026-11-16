package mcpserver

// 审批"记住允许"持久化的单元测试：Approver 与 Store 的接线行为。

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newPersistTestApprover 构造带持久化的 Approver（模拟 Runtime 的接线）。
func newPersistTestApprover(t *testing.T) (*Approver, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-settings.json")
	store := OpenStore(path)

	ap := NewApprover()
	ap.SetNotifier(func(ApprovalRequest) bool { return true }) // 测试环境假装有 UI
	ap.SetPersist(func(list []string) {
		_, _ = store.Update(func(s *Settings) error {
			s.Approval.Remembered = append([]string{}, list...)
			return nil
		})
	})
	ap.LoadRemembered(store.Get().Approval.Remembered)
	return ap, store
}

func TestApprovalRememberPersists(t *testing.T) {
	ap, store := newPersistTestApprover(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = ap.Request(context.Background(), "clear_app_data", "本地", "{}", 5*time.Second)
	}()

	// 等 UI 推送后裁决为"允许 + 记住"。
	deadline := time.Now().Add(2 * time.Second)
	for len(ap.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(ap.Pending()) != 1 {
		t.Fatalf("审批请求未进入队列")
	}
	if err := ap.Resolve(ap.Pending()[0].ID, ApprovalDecision{Allow: true, Remember: true}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wg.Wait()

	// 内存与持久层都要有。
	if got := ap.Remembered(); len(got) != 1 || got[0] != "clear_app_data" {
		t.Fatalf("内存记住清单 = %v, want [clear_app_data]", got)
	}
	if got := store.Get().Approval.Remembered; len(got) != 1 || got[0] != "clear_app_data" {
		t.Fatalf("落盘记住清单 = %v, want [clear_app_data]", got)
	}

	// 模拟重启：新 Approver + 新 Store 从同一文件恢复。
	reborn := OpenStore(store.Path())
	ap2 := NewApprover()
	ap2.LoadRemembered(reborn.Get().Approval.Remembered)
	if got := ap2.Remembered(); len(got) != 1 || got[0] != "clear_app_data" {
		t.Fatalf("重启后恢复的记住清单 = %v, want [clear_app_data]", got)
	}
}

func TestApprovalClearRememberedPersists(t *testing.T) {
	ap, store := newPersistTestApprover(t)
	ap.LoadRemembered([]string{"install_app"})
	ap.SetPersist(func(list []string) {
		_, _ = store.Update(func(s *Settings) error {
			s.Approval.Remembered = append([]string{}, list...)
			return nil
		})
	})

	ap.ClearRemembered()
	if got := store.Get().Approval.Remembered; len(got) != 0 {
		t.Fatalf("清空后落盘清单 = %v, want 空", got)
	}
	if got := ap.Remembered(); len(got) != 0 {
		t.Fatalf("清空后内存清单 = %v, want 空", got)
	}
}

func TestSettingsRememberedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-settings.json")
	store := OpenStore(path)
	if _, err := store.Update(func(s *Settings) error {
		s.Approval.Remembered = []string{"a", "b", " a "}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// normalize 会去重去空白。
	if got := store.Get().Approval.Remembered; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Remembered = %v, want [a b]", got)
	}

	// 重新载入文件仍保留。
	reborn := OpenStore(path)
	if got := reborn.Get().Approval.Remembered; len(got) != 2 {
		t.Fatalf("重启后 Remembered = %v, want [a b]", got)
	}
}
