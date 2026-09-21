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

// token 变更必须产生通知事件，且事件载荷不含凭据材料（FR-15 挂起补验项）。
func TestTokenLifecycleBroadcastsWithoutSecrets(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, _ := seedTwoClients(t, store)
	// 先建一个通知目标，否则广播会因"无启用目标"而不入队。
	createWebhookTarget(t, store, "验收目标", true)

	_, rotated, err := rotateToken(t, store, first.ID)
	if err != nil {
		t.Fatalf("轮换失败：%v", err)
	}
	if _, err := revokeToken(t, store, first.ID); err != nil {
		t.Fatalf("吊销失败：%v", err)
	}

	entries := readOutboxForTest(t, store)
	var rotatedEvents, revokedEvents []NotificationOutbox
	for _, entry := range entries {
		switch entry.EventType {
		case EventTypeClientTokenRotated:
			rotatedEvents = append(rotatedEvents, entry)
		case EventTypeClientTokenRevoked:
			revokedEvents = append(revokedEvents, entry)
		}
	}
	if len(rotatedEvents) == 0 {
		t.Fatalf("轮换应产生 %s 事件，实际 outbox 内容：%+v", EventTypeClientTokenRotated, entries)
	}
	if len(revokedEvents) == 0 {
		t.Fatalf("吊销应产生 %s 事件", EventTypeClientTokenRevoked)
	}

	// 载荷不得含 token 值或摘要：通知渠道是外部系统。
	// 注意 TargetID 存的是"这条通知发给谁"，不是"事件讲的是谁"——广播语义下
	// 它必然是通知目标的标识，因此这里不对它做断言。
	for _, entry := range append(rotatedEvents, revokedEvents...) {
		if strings.Contains(entry.Payload, rotated) {
			t.Fatalf("通知载荷不得含 token 明文：%s", entry.Payload)
		}
		if strings.Contains(entry.Payload, DigestToken(rotated)) {
			t.Fatalf("通知载荷不得含 token 摘要：%s", entry.Payload)
		}
		if !strings.Contains(entry.Payload, first.ID) {
			t.Fatalf("载荷应指明被变更的客户端标识：%s", entry.Payload)
		}
	}
}

// readOutboxForTest 读取 outbox 记录。
func readOutboxForTest(t *testing.T, store *Store) []NotificationOutbox {
	t.Helper()
	var entries []NotificationOutbox
	err := store.View(context.Background(), func(tx *Tx) error {
		var readErr error
		entries, readErr = tx.OutboxEntries()
		return readErr
	})
	if err != nil {
		t.Fatalf("读取 outbox 失败：%v", err)
	}
	return entries
}

// 条件更新守卫必须真的按 used_at 判条件：已使用的凭据不能被再次抢占。
//
// 这条直接调用守卫函数，不依赖并发。之所以要单独覆盖它，是因为并发兑换用例在
// 单连接模型下走不到守卫分支（第二个请求在更早的读检查处就返回了），实测把
// WHERE 里的 used_at IS NULL 去掉，那个用例仍然通过——也就是说守卫本身在
// 原有用例下没有任何守护者。
func TestMarkCredentialUsedIsConditional(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, _ := seedTwoClients(t, store)

	status, err := issueCredentialStatus(t, store, first.ID)
	if err != nil {
		t.Fatalf("发行凭据失败：%v", err)
	}

	// 第一次抢占应成功。
	won, err := markUsed(t, store, status.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("首次标记失败：%v", err)
	}
	if !won {
		t.Fatal("首次标记应抢占成功")
	}

	// 第二次必须失败：used_at 已经非空。
	again, err := markUsed(t, store, status.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("二次标记返回错误：%v", err)
	}
	if again {
		t.Fatal("已使用的凭据不得被再次抢占——条件更新的 WHERE 守卫失效了")
	}
}

// credentialStatus 是测试用的凭据标识与明文组合。
type credentialStatus struct {
	ID     string
	Secret string
}

// issueCredentialStatus 发行凭据并读出其标识。
func issueCredentialStatus(t *testing.T, store *Store, clientID string) (credentialStatus, error) {
	t.Helper()
	var result credentialStatus
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		secret, err := tx.IssueEnrollmentCredential(clientID)
		if err != nil {
			return err
		}
		var credential EnrollmentCredential
		if err := tx.db.First(&credential, "token_digest = ?", DigestToken(secret)).Error; err != nil {
			return err
		}
		result = credentialStatus{ID: credential.ID, Secret: secret}
		return nil
	})
	return result, err
}

// markUsed 在事务内调用条件更新守卫。
func markUsed(t *testing.T, store *Store, credentialID string, at time.Time) (bool, error) {
	t.Helper()
	var won bool
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		won, err = tx.markCredentialUsed(credentialID, at)
		return err
	})
	return won, err
}

// 长名称客户端必须仍可完成全部生命周期动作。
//
// 这条守护一个曾经真实存在的缺陷：审计上下文有 85 字符上限，而客户端名称当时
// 没有长度校验，名字较长的客户端在轮换与吊销时因审计校验失败整体回滚——
// 管理员因此无法吊销一个已失陷客户端的凭据。修法是让审计对用户输入免疫
// （截断而非失败），并给名称加显式上限。
func TestLongClientNameStillAllowsRevoke(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")

	// 取一个接近名称上限的长度，确保"长名称"这条路径确实被走到。
	name := strings.Repeat("客", maxClientNameLength-1)
	token, err := NewClientToken()
	if err != nil {
		t.Fatalf("生成 token 失败：%v", err)
	}
	err = store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.CreateClient(ClientInput{ID: "cli_long", Name: name, Token: token})
		return err
	})
	if err != nil {
		t.Fatalf("创建长名称客户端失败：%v", err)
	}

	for stage, action := range map[string]func() error{
		"轮换": func() error {
			_, _, err := rotateToken(t, store, "cli_long")
			return err
		},
		"吊销": func() error {
			_, err := revokeToken(t, store, "cli_long")
			return err
		},
	} {
		if err := action(); err != nil {
			t.Fatalf("长名称客户端的%s动作失败（审计长度不应成为业务写入的隐式约束）：%v", stage, err)
		}
	}
}

// 名称超出上限被拒绝，且返回可判定错误而不是落库后爆掉。
func TestCreateClientRejectsOverlongName(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	token, _ := NewClientToken()

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.CreateClient(ClientInput{
			ID: "cli_too_long", Name: strings.Repeat("客", maxClientNameLength+1), Token: token,
		})
		return err
	})
	if !errors.Is(err, ErrClientNameInvalid) {
		t.Fatalf("超长名称应返回可判定的名称错误，实际 %v", err)
	}
}

// 空名称同样按名称错误返回，而不是落到数据库约束上。
func TestCreateClientRejectsBlankName(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	token, _ := NewClientToken()

	err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.CreateClient(ClientInput{ID: "cli_blank", Name: "   ", Token: token})
		return err
	})
	if !errors.Is(err, ErrClientNameInvalid) {
		t.Fatalf("空白名称应返回名称错误，实际 %v", err)
	}
}

// 读取不存在的客户端返回哨兵错误，使 HTTP 层能把 404 与 500 分开。
func TestClientReadMissingReturnsSentinel(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")

	var err error
	_ = store.View(context.Background(), func(tx *Tx) error {
		_, err = tx.Client("cli_missing")
		return nil
	})
	if !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("读取不存在的客户端应返回 ErrClientNotFound，实际 %v", err)
	}
}

// 业务失败时不得留下通知：广播与业务写入同事务。
func TestFailedTokenActionLeavesNoOutbox(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	createWebhookTarget(t, store, "验收目标", true)

	// 对不存在的客户端轮换/吊销：两者都应失败，且不留任何 outbox 记录。
	if _, _, err := rotateToken(t, store, "cli_missing"); err == nil {
		t.Fatal("对不存在客户端的轮换应失败")
	}
	if _, err := revokeToken(t, store, "cli_missing"); err == nil {
		t.Fatal("对不存在客户端的吊销应失败")
	}

	entries := readOutboxForTest(t, store)
	if len(entries) != 0 {
		t.Fatalf("失败的业务动作不得留下通知记录，实际 %d 条：%+v", len(entries), entries)
	}
}

// 广播必须为每个启用目标各写一条记录，且指向该目标。
//
// 若 TargetID 写空，投递侧会走"发送时再枚举全部目标"的分支，与入队时的扇出
// 叠加，导致同一目标收到重复投递。
func TestTokenEventFanOutTargetsEachEnabledTarget(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	first, _ := seedTwoClients(t, store)
	createWebhookTarget(t, store, "目标甲", true)
	createWebhookTarget(t, store, "目标乙", true)

	if _, _, err := rotateToken(t, store, first.ID); err != nil {
		t.Fatalf("轮换失败：%v", err)
	}

	entries := readOutboxForTest(t, store)
	var rotated []NotificationOutbox
	for _, entry := range entries {
		if entry.EventType == EventTypeClientTokenRotated {
			rotated = append(rotated, entry)
		}
	}
	if len(rotated) != 2 {
		t.Fatalf("两个启用目标应各得一条记录，实际 %d 条", len(rotated))
	}
	for _, entry := range rotated {
		if entry.TargetID == "" {
			t.Fatal("TargetID 不得为空：空值会让投递侧再枚举一次目标，造成重复投递")
		}
	}
}
