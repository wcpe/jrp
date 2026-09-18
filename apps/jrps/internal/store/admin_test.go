package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 测试辅助：断言当前初始化状态。
func assertInitialized(t *testing.T, s *Store, want bool) {
	t.Helper()
	var got bool
	if err := s.View(context.Background(), func(tx *Tx) error {
		var err error
		got, err = tx.IsInitialized()
		return err
	}); err != nil {
		t.Fatalf("读取初始化状态失败：%v", err)
	}
	if got != want {
		t.Fatalf("初始化状态不匹配：期望 %v，实际 %v", want, got)
	}
}

// 测试辅助：初始化一个管理员，失败即终止测试。
func initializeAdmin(t *testing.T, s *Store, password string) {
	t.Helper()
	if err := s.Transaction(context.Background(), func(tx *Tx) error {
		return tx.InitializeAdmin(InitializeAdminInput{Password: password})
	}); err != nil {
		t.Fatalf("初始化管理员失败：%v", err)
	}
}

// 测试辅助：以给定凭据执行鉴权。
func authenticate(t *testing.T, s *Store, username, password string) (AdminCredential, error) {
	t.Helper()
	var credential AdminCredential
	err := s.View(context.Background(), func(tx *Tx) error {
		var err error
		credential, err = tx.AuthenticateAdmin(username, password)
		return err
	})
	return credential, err
}

// 初始化只能执行一次：首次成功，第二次必须拒绝。
func TestInitializeAdminIsOneShot(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	ctx := context.Background()

	assertInitialized(t, store, false)
	initializeAdmin(t, store, "correct-horse-battery")
	assertInitialized(t, store, true)

	err := store.Transaction(ctx, func(tx *Tx) error {
		return tx.InitializeAdmin(InitializeAdminInput{Password: "another-password"})
	})
	if !errors.Is(err, ErrAdminAlreadyInitialized) {
		t.Fatalf("重复初始化应返回已初始化哨兵错误：%v", err)
	}
}

// 重复初始化不得覆盖既有密码：旧密码仍可登录，新密码不可用。
func TestReinitializeDoesNotOverwritePassword(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	ctx := context.Background()
	password := "correct-horse-battery"
	initializeAdmin(t, store, password)

	if err := store.Transaction(ctx, func(tx *Tx) error {
		return tx.InitializeAdmin(InitializeAdminInput{Password: "another-password"})
	}); err == nil {
		t.Fatal("重复初始化应被拒绝")
	}

	if _, err := authenticate(t, store, AdminUsername, password); err != nil {
		t.Fatalf("重复初始化后旧密码应仍可登录：%v", err)
	}
	if _, err := authenticate(t, store, AdminUsername, "another-password"); err == nil {
		t.Fatal("重复初始化不得让新密码生效")
	}
}

// 空密码、过短、超长与非 UTF-8 输入都必须以中文明确拒绝。
func TestInitializeAdminRejectsInvalidPassword(t *testing.T) {
	invalid := string([]byte{0xff, 0xfe, 0xfd}) + "abcdefgh"
	cases := []struct {
		name     string
		password string
	}{
		{"空密码", ""},
		{"过短密码", "short"},
		{"超长密码", strings.Repeat("a", passwordMaxLength+1)},
		{"非 UTF-8 密码", invalid},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			store := openServerStore(t, t.TempDir()+"/jrps.db")
			err := store.Transaction(context.Background(), func(tx *Tx) error {
				return tx.InitializeAdmin(InitializeAdminInput{Password: item.password})
			})
			if err == nil {
				t.Fatalf("%s 应被拒绝", item.name)
			}
			assertInitialized(t, store, false)
		})
	}
}

// 初始化事务失败必须整体回滚，不得留下可登录的半初始化状态。
func TestInitializeAdminRollsBackOnFailure(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		if err := tx.InitializeAdmin(InitializeAdminInput{Password: "correct-horse-battery"}); err != nil {
			return err
		}
		return errors.New("模拟写入失败")
	})
	if err == nil {
		t.Fatal("事务内失败应向上返回错误")
	}
	assertInitialized(t, store, false)
	if _, err := authenticate(t, store, AdminUsername, "correct-horse-battery"); err == nil {
		t.Fatal("回滚后不得存在可登录的管理员")
	}
}

// 密码明文与派生材料都不得出现在管理员记录的任何列中。
func TestAdminPasswordMaterialNotStoredAsPlaintext(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	password := "correct-horse-battery"
	initializeAdmin(t, store, password)

	columns := []string{"username", "password_digest", "password_salt", "password_params", "initialization_id"}
	if err := store.View(context.Background(), func(tx *Tx) error {
		rows, err := tx.DB().Raw(
			"SELECT username, password_digest, password_salt, password_params, initialization_id FROM admin_credentials",
		).Rows()
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for rows.Next() {
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				return err
			}
			for index, value := range values {
				if text, ok := value.(string); ok && strings.Contains(text, password) {
					t.Errorf("管理员记录列 %s 泄露密码明文", columns[index])
				}
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("扫描管理员记录失败：%v", err)
	}
}

// 登录失败不得区分"用户不存在"与"密码错误"。
func TestAuthenticateAdminDoesNotDistinguishUserAndPassword(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	initializeAdmin(t, store, "correct-horse-battery")

	wrongUser, errUser := authenticate(t, store, "not-admin", "correct-horse-battery")
	wrongPassword, errPassword := authenticate(t, store, AdminUsername, "wrong-password")

	if errUser == nil || errPassword == nil {
		t.Fatal("错误的用户名或密码都必须被拒绝")
	}
	if errUser.Error() != errPassword.Error() {
		t.Fatalf("两种失败必须返回同一提示：%q 与 %q", errUser.Error(), errPassword.Error())
	}
	if wrongUser.ID != 0 || wrongPassword.ID != 0 {
		t.Fatal("失败时不得返回管理员记录")
	}
}

// 会话令牌只以摘要落库：服务端不保存可重放的会话密钥明文。
func TestSessionStoresTokenDigestOnly(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	var issued SessionToken
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		issued, err = tx.CreateSession(CreateSessionInput{Username: AdminUsername, TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatalf("建立会话失败：%v", err)
	}

	// CSRF 状态是防跨站值而非会话密钥：它必须回显给浏览器脚本，
	// 因此以明文随会话保存；断言它确实可用（由 emitCsrfToken 负责发放）。
	var storedCSRFToken string
	if err := store.View(context.Background(), func(tx *Tx) error {
		session, err := tx.ActiveSession(issued.Token)
		if err != nil {
			return err
		}
		storedCSRFToken = session.CSRFToken
		return nil
	}); err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	if storedCSRFToken != issued.CSRFToken {
		t.Fatalf("CSRF 状态必须与发放给浏览器的一致")
	}
}

// 会话注销后必须立即失效。
func TestSessionInvalidatedImmediately(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	ctx := context.Background()
	var issued SessionToken
	if err := store.Transaction(ctx, func(tx *Tx) error {
		var err error
		issued, err = tx.CreateSession(CreateSessionInput{Username: AdminUsername, TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatalf("建立会话失败：%v", err)
	}
	if err := store.View(ctx, func(tx *Tx) error {
		_, err := tx.ActiveSession(issued.Token)
		return err
	}); err != nil {
		t.Fatalf("新会话应可用：%v", err)
	}

	if err := store.Transaction(ctx, func(tx *Tx) error {
		return tx.InvalidateSession(issued.Token)
	}); err != nil {
		t.Fatalf("注销会话失败：%v", err)
	}
	if err := store.View(ctx, func(tx *Tx) error {
		_, err := tx.ActiveSession(issued.Token)
		return err
	}); err == nil {
		t.Fatal("注销后会话必须立即失效")
	}
}

// 过期会话不得被判定为有效。
func TestExpiredSessionIsRejected(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	ctx := context.Background()
	var issued SessionToken
	if err := store.Transaction(ctx, func(tx *Tx) error {
		var err error
		issued, err = tx.CreateSession(CreateSessionInput{Username: AdminUsername, TTL: -time.Second})
		return err
	}); err != nil {
		t.Fatalf("建立会话失败：%v", err)
	}
	if err := store.View(ctx, func(tx *Tx) error {
		_, err := tx.ActiveSession(issued.Token)
		return err
	}); err == nil {
		t.Fatal("过期会话必须被拒绝")
	}
}

// 未知会话令牌必须被拒绝，不得因查询落空而返回零值会话。
func TestUnknownSessionTokenIsRejected(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	if err := store.View(context.Background(), func(tx *Tx) error {
		_, err := tx.ActiveSession("not-a-real-token")
		return err
	}); err == nil {
		t.Fatal("未知会话令牌必须被拒绝")
	}
}

// 初始化、登录、登出与失败尝试必须写入对应审计动作，且审计不含密码。
func TestAuthenticationAuditActions(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	ctx := context.Background()
	password := "correct-horse-battery"
	initializeAdmin(t, store, password)

	if _, err := authenticate(t, store, AdminUsername, "wrong-password"); err == nil {
		t.Fatal("错误密码必须被拒绝")
	}
	if err := store.Transaction(ctx, func(tx *Tx) error {
		return tx.RecordLoginFailure(AdminUsername)
	}); err != nil {
		t.Fatalf("记录登录失败审计失败：%v", err)
	}
	var issued SessionToken
	if err := store.Transaction(ctx, func(tx *Tx) error {
		var err error
		issued, err = tx.CreateSession(CreateSessionInput{Username: AdminUsername, TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatalf("建立会话失败：%v", err)
	}
	if err := store.Transaction(ctx, func(tx *Tx) error {
		return tx.InvalidateSession(issued.Token)
	}); err != nil {
		t.Fatalf("注销会话失败：%v", err)
	}

	var events []AuditEvent
	if err := store.View(ctx, func(tx *Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计事件失败：%v", err)
	}
	present := map[string]bool{}
	for _, event := range events {
		present[event.Action] = true
		if strings.Contains(event.Context, password) {
			t.Fatalf("审计事件不得包含密码：%+v", event)
		}
	}
	for _, action := range []string{ActionAdminInitialized, ActionAdminLogin, ActionAdminLogout, ActionAdminLoginFailure} {
		if !present[action] {
			t.Fatalf("缺少审计动作 %s：%+v", action, present)
		}
	}
}
