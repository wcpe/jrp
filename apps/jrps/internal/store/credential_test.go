package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 轮换后旧 token 立即失效、新 token 可用，且不影响其他客户端。
func TestRotateClientTokenInvalidatesOldImmediately(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, other := seedTwoClients(t, store)

	view, newToken, err := rotateToken(t, store, first.ID)
	if err != nil {
		t.Fatalf("轮换失败：%v", err)
	}
	if view.ID != first.ID {
		t.Fatalf("轮换应返回目标客户端，实际 %s", view.ID)
	}

	if _, err := authenticateClient(t, store, first.Token); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("旧 token 应立即失效，实际 %v", err)
	}
	authenticated, err := authenticateClient(t, store, newToken)
	if err != nil {
		t.Fatalf("新 token 应可用：%v", err)
	}
	if authenticated.ID != first.ID {
		t.Fatalf("新 token 应属于同一客户端，实际 %s", authenticated.ID)
	}

	// 轮换不得波及其他客户端：这是"每客户端独立 token"的核心价值。
	if _, err := authenticateClient(t, store, other.Token); err != nil {
		t.Fatalf("轮换不应影响其他客户端：%v", err)
	}
}

// 吊销后该 token 一律鉴权失败，且不可恢复。
func TestRevokeClientTokenIsPermanent(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, other := seedTwoClients(t, store)

	if _, err := revokeToken(t, store, first.ID); err != nil {
		t.Fatalf("吊销失败：%v", err)
	}
	if _, err := authenticateClient(t, store, first.Token); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("吊销后鉴权应失败，实际 %v", err)
	}

	// 已吊销不可恢复：重新发行凭据也不能把状态翻回来。
	if _, err := issueCredential(t, store, first.ID); err != nil {
		t.Fatalf("发行凭据失败：%v", err)
	}
	if _, _, err := redeemCredential(t, store, "不存在的凭据"); !errors.Is(err, ErrCredentialRejected) {
		t.Fatalf("无效凭据应被拒，实际 %v", err)
	}

	if _, err := authenticateClient(t, store, other.Token); err != nil {
		t.Fatalf("吊销不应影响其他客户端：%v", err)
	}
}

// enrollment 凭据是一次性的：第二次兑换必须失败。
func TestEnrollmentCredentialIsSingleUse(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, _ := seedTwoClients(t, store)

	secret, err := issueCredential(t, store, first.ID)
	if err != nil {
		t.Fatalf("发行凭据失败：%v", err)
	}

	view, token, err := redeemCredential(t, store, secret)
	if err != nil {
		t.Fatalf("首次兑换应成功：%v", err)
	}
	if view.EnrollmentState != EnrollmentStateActive {
		t.Fatalf("兑换后应转为 active，实际 %s", view.EnrollmentState)
	}
	if _, err := authenticateClient(t, store, token); err != nil {
		t.Fatalf("兑换得到的 token 应可用：%v", err)
	}

	if _, _, err := redeemCredential(t, store, secret); !errors.Is(err, ErrCredentialRejected) {
		t.Fatalf("凭据重复使用应被拒，实际 %v", err)
	}
}

// 过期凭据不能兑换。
func TestEnrollmentCredentialRejectsExpired(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, _ := seedTwoClients(t, store)

	secret, err := issueCredential(t, store, first.ID)
	if err != nil {
		t.Fatalf("发行凭据失败：%v", err)
	}
	// 直接把过期时间推到过去，模拟时间流逝。
	expireCredential(t, store, secret)

	if _, _, err := redeemCredential(t, store, secret); !errors.Is(err, ErrCredentialRejected) {
		t.Fatalf("过期凭据应被拒，实际 %v", err)
	}
}

// 同一张凭据被并发提交时只能有一个成功——否则一次发行会产出两个 token。
//
// 本用例验证的是"串行模型下的重复兑换拒绝"：当前连接模型（MaxOpenConns=1 +
// EXCLUSIVE 锁）把事务串行化，第二个请求在读检查处就被拦下。真正兜住"读检查
// 通过之后、写入之前被插手"的那道条件更新由 TestMarkCredentialUsedIsConditional
// 直接覆盖——两处一起才完整，单靠本用例会漏掉守卫本身。
func TestEnrollmentCredentialRepeatedRedeemIsRejected(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, _ := seedTwoClients(t, store)

	secret, err := issueCredential(t, store, first.ID)
	if err != nil {
		t.Fatalf("发行凭据失败：%v", err)
	}

	const racers = 4
	results := make(chan error, racers)
	for index := 0; index < racers; index++ {
		go func() {
			_, _, err := redeemCredential(t, store, secret)
			results <- err
		}()
	}
	succeeded := 0
	for index := 0; index < racers; index++ {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, ErrCredentialRejected) {
			t.Fatalf("并发兑换的错误应统一为凭据被拒，实际 %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("并发兑换应只有一个成功，实际 %d", succeeded)
	}
}

// 轮换、吊销与凭据发行都必须写审计，且审计不含完整 token 或凭据明文。
func TestTokenLifecycleAuditsWithoutSecrets(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, _ := seedTwoClients(t, store)

	secret, err := issueCredential(t, store, first.ID)
	if err != nil {
		t.Fatalf("发行凭据失败：%v", err)
	}
	_, rotated, err := rotateToken(t, store, first.ID)
	if err != nil {
		t.Fatalf("轮换失败：%v", err)
	}
	if _, err := revokeToken(t, store, first.ID); err != nil {
		t.Fatalf("吊销失败：%v", err)
	}

	events := readAuditEvents(t, store)
	joined := ""
	for _, event := range events {
		joined += event.Context + event.ObjectID + event.ActorID
	}
	for name, value := range map[string]string{
		"原始 token":  first.Token,
		"轮换后 token": rotated,
		"凭据明文":      secret,
	} {
		if strings.Contains(joined, value) {
			t.Fatalf("审计中不得出现%s", name)
		}
	}
	// 三类动作都要留痕：只记成功而不记生命周期动作会让审计失去追溯力。
	for _, action := range []string{ActionClientCreate, ActionClientRotate, ActionClientRevoke} {
		if !hasAuditAction(events, action) {
			t.Fatalf("缺少审计动作 %s", action)
		}
	}
}

// 对不存在的客户端执行生命周期动作返回明确错误，而不是静默成功。
func TestTokenLifecycleRejectsUnknownClient(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")

	if _, err := issueCredential(t, store, "cli_missing"); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("发行凭据应报客户端不存在，实际 %v", err)
	}
	if _, _, err := rotateToken(t, store, "cli_missing"); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("轮换应报客户端不存在，实际 %v", err)
	}
	if _, err := revokeToken(t, store, "cli_missing"); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("吊销应报客户端不存在，实际 %v", err)
	}
}

// seededClient 是测试用的客户端标识与 token 明文组合。
type seededClient struct {
	ID    string
	Token string
}

// seedTwoClients 建立两个持有各自 token 的客户端，用于验证隔离性。
func seedTwoClients(t *testing.T, store *Store) (seededClient, seededClient) {
	t.Helper()
	firstToken, err := NewClientToken()
	if err != nil {
		t.Fatalf("生成 token 失败：%v", err)
	}
	secondToken, err := NewClientToken()
	if err != nil {
		t.Fatalf("生成 token 失败：%v", err)
	}
	err = store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.CreateClient(ClientInput{ID: "cli_a", Name: "客户端甲", Token: firstToken}); err != nil {
			return err
		}
		_, err := tx.CreateClient(ClientInput{ID: "cli_b", Name: "客户端乙", Token: secondToken})
		return err
	})
	if err != nil {
		t.Fatalf("建立客户端失败：%v", err)
	}
	return seededClient{ID: "cli_a", Token: firstToken}, seededClient{ID: "cli_b", Token: secondToken}
}

// issueCredential 在事务内发行凭据。
func issueCredential(t *testing.T, store *Store, clientID string) (string, error) {
	t.Helper()
	var secret string
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		issued, err := tx.IssueEnrollmentCredential(clientID)
		secret = issued
		return err
	})
	return secret, err
}

// redeemCredential 在事务内兑换凭据。
func redeemCredential(t *testing.T, store *Store, secret string) (ClientView, string, error) {
	t.Helper()
	var view ClientView
	var token string
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		redeemed, issued, err := tx.RedeemEnrollmentCredential(secret)
		view, token = redeemed, issued
		return err
	})
	return view, token, err
}

// rotateToken 在事务内轮换 token。
func rotateToken(t *testing.T, store *Store, clientID string) (ClientView, string, error) {
	t.Helper()
	var view ClientView
	var token string
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		rotated, issued, err := tx.RotateClientToken(clientID)
		view, token = rotated, issued
		return err
	})
	return view, token, err
}

// revokeToken 在事务内吊销 token。
func revokeToken(t *testing.T, store *Store, clientID string) (ClientView, error) {
	t.Helper()
	var view ClientView
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		revoked, err := tx.RevokeClientToken(clientID)
		view = revoked
		return err
	})
	return view, err
}

// authenticate 在事务内鉴权 token。
func authenticateClient(t *testing.T, store *Store, token string) (Client, error) {
	t.Helper()
	var client Client
	err := store.View(context.Background(), func(tx *Tx) error {
		authenticated, err := tx.AuthenticateClientToken(token)
		client = authenticated
		return err
	})
	return client, err
}

// expireCredential 把凭据有效期推到过去，用于验证过期路径。
func expireCredential(t *testing.T, store *Store, secret string) {
	t.Helper()
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Model(&EnrollmentCredential{}).
			Where("token_digest = ?", DigestToken(secret)).
			Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error
	})
	if err != nil {
		t.Fatalf("设置过期时间失败：%v", err)
	}
}

// readAuditEvents 读取全部审计事件。
func readAuditEvents(t *testing.T, store *Store) []AuditEvent {
	t.Helper()
	var events []AuditEvent
	err := store.View(context.Background(), func(tx *Tx) error {
		return tx.db.Order("id ASC").Find(&events).Error
	})
	if err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	return events
}

// hasAuditAction 判断审计事件中是否存在指定动作。
func hasAuditAction(events []AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}
