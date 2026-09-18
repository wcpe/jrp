package store

import (
	"context"
	"strings"
	"testing"
)

// token 只以摘要落库：完整明文不得出现在任何列中。
func TestTokenStoredAsDigestOnly(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	token, err := NewClientToken()
	if err != nil {
		t.Fatalf("生成 token 失败：%v", err)
	}

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.CreateClient(ClientInput{ID: "client-1", Name: "测试客户端", Token: token})
		return err
	}); err != nil {
		t.Fatalf("创建客户端失败：%v", err)
	}

	// 穷举客户端的全部业务列，确认不存在 token 明文。
	columns := storedTokenColumns()
	err = store.View(context.Background(), func(tx *Tx) error {
		rows, err := tx.DB().Raw("SELECT id, name, token_digest, enrollment_state, connection_state FROM clients").Rows()
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
				if text, ok := value.(string); ok && strings.Contains(text, token) {
					return errTokenLeaked(columns[index])
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("列扫描失败：%v", err)
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		var client Client
		if err := tx.DB().First(&client, "id = ?", "client-1").Error; err != nil {
			return err
		}
		if client.TokenDigest != DigestToken(token) {
			t.Fatalf("落库内容应为 token 摘要：%s", client.TokenDigest)
		}
		return nil
	}); err != nil {
		t.Fatalf("校验摘要失败：%v", err)
	}
}

// 读取接口只返回掩码与元数据，不含完整 token 或完整摘要。
func TestClientViewReturnsMaskedTokenOnly(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	token, err := NewClientToken()
	if err != nil {
		t.Fatalf("生成 token 失败：%v", err)
	}
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.CreateClient(ClientInput{ID: "client-1", Name: "客户端", Token: token})
		return err
	}); err != nil {
		t.Fatalf("创建客户端失败：%v", err)
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		view, err := tx.Client("client-1")
		if err != nil {
			return err
		}
		if containsTokenPlaintext(view.MaskedToken(), token) {
			return errTokenLeaked("掩码输出")
		}
		if !strings.HasPrefix(view.MaskedToken(), "****") {
			return errTokenLeaked("掩码格式")
		}
		views, err := tx.Clients()
		if err != nil {
			return err
		}
		for _, item := range views {
			if containsTokenPlaintext(item.MaskedToken(), token) {
				return errTokenLeaked("列表输出")
			}
			if strings.Contains(item.DigestPrefix, DigestToken(token)) {
				return errTokenLeaked("列表摘要前缀")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("读取视图失败：%v", err)
	}
}

// 审计事件不得包含 token 明文或完整摘要，只允许摘要短前缀。
func TestAuditEventsDoNotContainToken(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	token, err := NewClientToken()
	if err != nil {
		t.Fatalf("生成 token 失败：%v", err)
	}
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.CreateClient(ClientInput{ID: "client-1", Name: "客户端", Token: token})
		return err
	}); err != nil {
		t.Fatalf("创建客户端失败：%v", err)
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		events, err := tx.AuditEvents()
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return errTokenLeaked("缺少审计事件")
		}
		for _, event := range events {
			combined := event.Context + event.ObjectID + event.ActorID
			if containsTokenPlaintext(combined, token) {
				return errTokenLeaked("审计内容")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("审计脱敏校验失败：%v", err)
	}
}

// 鉴权只接受摘要匹配的 token；错误 token 与已吊销客户端一律被拒绝。
func TestAuthenticateClientToken(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	token, err := NewClientToken()
	if err != nil {
		t.Fatalf("生成 token 失败：%v", err)
	}
	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.CreateClient(ClientInput{ID: "client-1", Name: "客户端", Token: token})
		return err
	}); err != nil {
		t.Fatalf("创建客户端失败：%v", err)
	}

	if err := store.View(context.Background(), func(tx *Tx) error {
		client, err := tx.AuthenticateClientToken(token)
		if err != nil {
			return err
		}
		if client.ID != "client-1" {
			return errTokenLeaked("鉴权返回错误客户端")
		}
		if _, err := tx.AuthenticateClientToken("错误token"); err == nil {
			return errTokenLeaked("错误 token 应被拒绝")
		}
		return nil
	}); err != nil {
		t.Fatalf("鉴权校验失败：%v", err)
	}

	// 吊销后鉴权必须失败。
	if err := store.DB().Exec(
		"UPDATE clients SET enrollment_state = ? WHERE id = ?", EnrollmentStateRevoked, "client-1",
	).Error; err != nil {
		t.Fatalf("吊销客户端失败：%v", err)
	}
	if err := store.View(context.Background(), func(tx *Tx) error {
		if _, err := tx.AuthenticateClientToken(token); err == nil {
			return errTokenLeaked("已吊销客户端的 token 应被拒绝")
		}
		return nil
	}); err != nil {
		t.Fatalf("吊销校验失败：%v", err)
	}
}

// storedTokenColumns 返回客户端表中所有可能承载 token 的列，用于穷举脱敏断言。
func storedTokenColumns() []string {
	return []string{"id", "name", "token_digest", "enrollment_state", "connection_state"}
}

// containsTokenPlaintext 判断输出中是否出现给定的 token 明文或完整摘要。
func containsTokenPlaintext(output, token string) bool {
	if token == "" {
		return false
	}
	return strings.Contains(output, token) || strings.Contains(output, DigestToken(token))
}

type tokenLeakError struct{ field string }

func (e tokenLeakError) Error() string { return "检测到 token 泄露：" + e.field }

func errTokenLeaked(field string) error { return tokenLeakError{field: field} }
