package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 测试辅助：打开一个已初始化的 jrps 数据库。
func openInitializedServerStore(t *testing.T) *Store {
	t.Helper()
	database := openServerStore(t, filepath.Join(t.TempDir(), "jrps.db"))
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.InitializeAdmin(InitializeAdminInput{Password: "correct-horse-battery"})
	}); err != nil {
		t.Fatalf("初始化管理员失败：%v", err)
	}
	return database
}

// 测试辅助：在事务中写入一条审计事件。
func writeAuditInTransaction(t *testing.T, database *Store, event AuditEvent) error {
	t.Helper()
	return database.Transaction(context.Background(), func(tx *Tx) error {
		return tx.writeAudit(event)
	})
}

// 测试辅助：读取全部审计事件。
func mustAuditEvents(t *testing.T, database *Store) []AuditEvent {
	t.Helper()
	var events []AuditEvent
	if err := database.View(context.Background(), func(tx *Tx) error {
		var err error
		events, err = tx.AuditEvents()
		return err
	}); err != nil {
		t.Fatalf("读取审计事件失败：%v", err)
	}
	return events
}

// auditExpectation 描述对某条审计事件的期望。
type auditExpectation struct {
	Action     string
	ObjectType string
	Result     string
}

// 测试辅助：断言审计事件集合中存在满足期望的一条，并校验六项字段齐全。
func assertAuditEvent(t *testing.T, events []AuditEvent, expect auditExpectation) {
	t.Helper()
	for _, event := range events {
		if event.Action != expect.Action || event.ObjectType != expect.ObjectType {
			continue
		}
		if event.Result != expect.Result {
			t.Fatalf("审计结果不匹配：期望 %s，实际 %s", expect.Result, event.Result)
		}
		if event.ActorType == "" || event.ActorID == "" {
			t.Fatalf("审计主体字段不全：%+v", event)
		}
		if event.ObjectID == "" {
			t.Fatalf("审计对象标识为空：%+v", event)
		}
		if event.OccurredAt.IsZero() {
			t.Fatalf("审计时间缺失：%+v", event)
		}
		return
	}
	t.Fatalf("未找到期望的审计事件：%+v", expect)
}

// baseAuditEvent 返回一条字段齐全的合法审计事件，供各用例按需改写单个字段。
func baseAuditEvent() AuditEvent {
	return AuditEvent{
		ActorType:  ActorTypeAdmin,
		ActorID:    "admin",
		Action:     ActionAdminLogin,
		ObjectType: ObjectTypeSession,
		ObjectID:   "digest-prefix",
		Result:     AuditResultSuccess,
		Context:    "管理员登录成功，会话已建立",
	}
}

// 动作是封闭枚举：未登记的动作必须被拒绝，避免自由文本混入审计。
func TestAuditRejectsUnknownAction(t *testing.T) {
	database := openInitializedServerStore(t)
	event := baseAuditEvent()
	event.Action = "随便拼的动作"

	err := writeAuditInTransaction(t, database, event)
	if err == nil {
		t.Fatal("未登记的动作必须被拒绝")
	}
	if !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("应返回审计校验错误：%v", err)
	}
}

// 对象类型同样是封闭枚举（FR-16 规格 §2.1）。
func TestAuditRejectsUnknownObjectType(t *testing.T) {
	database := openInitializedServerStore(t)
	event := baseAuditEvent()
	event.ObjectType = "未登记的对象类型"

	if err := writeAuditInTransaction(t, database, event); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("未登记的对象类型必须被拒绝：%v", err)
	}
}

// 结果只能是成功、失败或被拒绝三值。
func TestAuditRejectsUnknownResult(t *testing.T) {
	database := openInitializedServerStore(t)
	event := baseAuditEvent()
	event.Result = "大概成功"

	if err := writeAuditInTransaction(t, database, event); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("非法结果必须被拒绝：%v", err)
	}
}

// 主体类别只能是管理员或客户端。
func TestAuditRejectsUnknownActorType(t *testing.T) {
	database := openInitializedServerStore(t)
	event := baseAuditEvent()
	event.ActorType = "root"

	if err := writeAuditInTransaction(t, database, event); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("非法主体类别必须被拒绝：%v", err)
	}
}

// 主体与对象标识不能为空：没有对象标识的审计无法回答"对谁做了什么"。
func TestAuditRejectsEmptyIdentifiers(t *testing.T) {
	database := openInitializedServerStore(t)

	missingActor := baseAuditEvent()
	missingActor.ActorID = "  "
	if err := writeAuditInTransaction(t, database, missingActor); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("空主体标识必须被拒绝：%v", err)
	}

	missingObject := baseAuditEvent()
	missingObject.ObjectID = ""
	if err := writeAuditInTransaction(t, database, missingObject); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("空对象标识必须被拒绝：%v", err)
	}
}

// 时间是服务端事实：调用方填的时间必须被忽略，不能采信客户端提供的时间。
func TestAuditOverridesCallerProvidedTime(t *testing.T) {
	database := openInitializedServerStore(t)
	forged := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	event := baseAuditEvent()
	event.OccurredAt = forged

	before := time.Now().UTC()
	if err := writeAuditInTransaction(t, database, event); err != nil {
		t.Fatalf("写入审计事件失败：%v", err)
	}
	after := time.Now().UTC()

	var stored AuditEvent
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Order("id DESC").First(&stored).Error
	}); err != nil {
		t.Fatalf("读取审计事件失败：%v", err)
	}
	if stored.OccurredAt.Equal(forged) {
		t.Fatal("调用方提供的时间不得被采信")
	}
	if stored.OccurredAt.Before(before.Add(-time.Second)) || stored.OccurredAt.After(after.Add(time.Second)) {
		t.Fatalf("审计时间应接近服务端当前时间，实际 %v", stored.OccurredAt)
	}
}

// 六类敏感值逐一构造用例：任一出现都必须被拒绝（FR-16 规格 §5 错误路径）。
func TestAuditRejectsSensitiveContext(t *testing.T) {
	database := openInitializedServerStore(t)
	cases := []struct {
		name    string
		context string
	}{
		{"完整令牌", "token=abcdef0123456789"},
		{"密码字段名", "password_digest=deadbeef"},
		{"密码盐", "password_salt=cafe1234"},
		{"授权头", "Authorization: Bearer abcdef"},
		{"Cookie", "Cookie: jrp_session=abcdef"},
		{"会话 Cookie 对", "jrp_session=abcdef0123"},
		{"Unix 分段路径", "分段文件位于 /home/jrp/segments/0001.seg"},
		{"Windows 分段路径", `分段文件位于 C:\jrp\segments\0001.seg`},
		{"私钥块", "-----BEGIN PRIVATE KEY-----"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			event := baseAuditEvent()
			event.Context = item.context
			err := writeAuditInTransaction(t, database, event)
			if err == nil {
				t.Fatalf("含敏感内容的上下文必须被拒绝：%s", item.context)
			}
			if !errors.Is(err, ErrAuditInvalid) {
				t.Fatalf("应返回审计校验错误：%v", err)
			}
		})
	}
}

// 错误信息本身不得回显被拒绝的敏感原文，否则校验失败反而把秘密写进了日志。
func TestAuditRejectionDoesNotEchoSecret(t *testing.T) {
	database := openInitializedServerStore(t)
	event := baseAuditEvent()
	event.Context = "password_digest=super-secret-value"

	err := writeAuditInTransaction(t, database, event)
	if err == nil {
		t.Fatal("含敏感内容的上下文必须被拒绝")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("校验错误不得回显敏感原文：%v", err)
	}
}

// 合规的"摘要前缀"写法必须放行：规格禁止的是完整秘密，不是描述动作的词语。
func TestAuditAllowsDigestPrefixWording(t *testing.T) {
	database := openInitializedServerStore(t)
	event := baseAuditEvent()
	event.Action = ActionClientCreate
	event.ObjectType = ObjectTypeClient
	event.ObjectID = "client-1"
	event.Context = "客户端 edge-1 已创建，token 摘要前缀 a1b2c3d4"

	if err := writeAuditInTransaction(t, database, event); err != nil {
		t.Fatalf("含摘要前缀的合规上下文应被放行：%v", err)
	}
}

// 上下文长度受限：超出列宽会被截断或写库失败，必须在写入前明确拒绝。
func TestAuditRejectsOverlongContext(t *testing.T) {
	database := openInitializedServerStore(t)
	event := baseAuditEvent()
	event.Context = strings.Repeat("超", maxAuditContextRunes+1)

	if err := writeAuditInTransaction(t, database, event); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("超长上下文必须被拒绝：%v", err)
	}
}

// 校验失败必须让业务事务整体回滚，不留下"业务成功但审计非法"的状态。
func TestAuditValidationFailureRollsBackBusinessChange(t *testing.T) {
	database := openInitializedServerStore(t)

	err := database.Transaction(context.Background(), func(tx *Tx) error {
		if err := tx.db.Create(&Client{
			ID:              "client-rollback",
			Name:            "回滚验证客户端",
			TokenDigest:     strings.Repeat("a", 64),
			EnrollmentState: EnrollmentStatePending,
			ConnectionState: ConnectionStateOffline,
		}).Error; err != nil {
			return err
		}
		event := baseAuditEvent()
		event.Action = "未登记的动作"
		return tx.writeAudit(event)
	})
	if err == nil {
		t.Fatal("审计校验失败必须让事务失败")
	}

	var count int64
	if err := database.View(context.Background(), func(tx *Tx) error {
		return tx.db.Model(&Client{}).Where("id = ?", "client-rollback").Count(&count).Error
	}); err != nil {
		t.Fatalf("统计客户端失败：%v", err)
	}
	if count != 0 {
		t.Fatal("审计校验失败时业务写入必须一并回滚")
	}
}

// 动作、对象类型与结果的封闭枚举必须覆盖 FR-16 §2.2 要求的全部动作。
func TestAuditActionEnumerationCoversSpec(t *testing.T) {
	required := []string{
		ActionAdminLogin, ActionAdminLogout, ActionAdminLoginFailure,
		ActionClientCreate, ActionClientRotate, ActionClientRevoke,
		ActionProxyCreate, ActionProxyUpdate, ActionProxyDelete, ActionRestoreApply,
		ActionApplyPrepare, ActionApplyPublish, ActionApplyFailure,
		ActionPolicyUpdate,
	}
	for _, action := range required {
		if _, ok := auditActions[action]; !ok {
			t.Fatalf("动作枚举缺少规格要求的取值：%s", action)
		}
	}

	requiredObjects := []string{
		ObjectTypeClient, ObjectTypeProxy, ObjectTypeConfigRevision, ObjectTypeToken,
		ObjectTypeCaptureRecord, ObjectTypeRetentionPolicy, ObjectTypeSession,
	}
	for _, objectType := range requiredObjects {
		if _, ok := objectTypes[objectType]; !ok {
			t.Fatalf("对象类型枚举缺少规格要求的取值：%s", objectType)
		}
	}
}
