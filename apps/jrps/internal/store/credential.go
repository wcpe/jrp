package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// enrollmentCredentialLifetime 是 enrollment 凭据的默认有效期。
//
// 规格 §6 把它列为待定项（需与 Web 交互确认）。此处取一个有界默认值：
// 凭据是一次性短凭据，有效期过长会放大泄漏窗口，过短会让首次部署反复失败。
const enrollmentCredentialLifetime = time.Hour

// ErrCredentialRejected 表示 enrollment 凭据无效、过期或已被使用。
//
// 与 ErrTokenRejected 同样对外不区分具体原因：区分"不存在"与"已过期"会让
// 攻击者据此判断凭据是否曾经存在（FR-07 §3.4）。
var ErrCredentialRejected = errors.New("enrollment 凭据无效或已失效")

// ErrClientNotFound 表示客户端不存在。
var ErrClientNotFound = errors.New("客户端不存在")

// issance 分隔线：token 生命周期实现。

// NewCredentialSecret 生成一个凭据明文；与客户端 token 同强度。
func NewCredentialSecret() (string, error) {
	buffer := make([]byte, tokenRawBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成 enrollment 凭据失败：%w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// IssueEnrollmentCredential 为目标客户端发行一张一次性凭据。
//
// 返回明文凭据，只在本次响应中出现一次；落库的只有摘要。同一客户端可以有多张
// 未使用的凭据（重新发行不是错误），但每张只能兑换一次。
func (tx *Tx) IssueEnrollmentCredential(clientID string) (string, error) {
	if clientID == "" {
		return "", errors.New("客户端标识不能为空")
	}
	secret, err := NewCredentialSecret()
	if err != nil {
		return "", err
	}
	credentialID, err := newCredentialID()
	if err != nil {
		return "", err
	}
	err = tx.Transaction(func() error {
		var client Client
		if err := tx.db.First(&client, "id = ?", clientID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrClientNotFound
			}
			return fmt.Errorf("读取客户端失败：%w", translateSQLError(err))
		}
		now := time.Now().UTC()
		credential := EnrollmentCredential{
			ID:          credentialID,
			ClientID:    clientID,
			TokenDigest: DigestToken(secret),
			ExpiresAt:   now.Add(enrollmentCredentialLifetime),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := tx.db.Create(&credential).Error; err != nil {
			return fmt.Errorf("写入 enrollment 凭据失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeAdmin,
			ActorID:    "admin",
			Action:     ActionClientCreate,
			ObjectType: ObjectTypeToken,
			ObjectID:   clientID,
			Result:     AuditResultSuccess,
			// 审计只记凭据标识与摘要短前缀，不记凭据明文。
			Context: fmt.Sprintf("已发行 enrollment 凭据 %s，摘要前缀 %s", credentialID, DigestPrefix(credential.TokenDigest)),
		})
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// RedeemEnrollmentCredential 用一次性凭据换取客户端 token。
//
// 兑换与凭据作废在同一事务完成：先标记凭据已用，再轮换客户端 token 并置为
// active。任一步失败整体回滚，不会出现"凭据已作废但拿不到 token"的中间态。
// 返回的 token 明文只在本次响应中出现一次。
func (tx *Tx) RedeemEnrollmentCredential(secret string) (ClientView, string, error) {
	if secret == "" {
		return ClientView{}, "", ErrCredentialRejected
	}
	digest := DigestToken(secret)
	var credential EnrollmentCredential
	var client Client
	var token string

	err := tx.Transaction(func() error {
		if err := tx.db.First(&credential, "token_digest = ?", digest).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCredentialRejected
			}
			return fmt.Errorf("查询 enrollment 凭据失败：%w", translateSQLError(err))
		}
		// 已使用与已过期是两种失效原因，但对外返回同一错误（§3.4）。
		if credential.UsedAt != nil {
			return ErrCredentialRejected
		}
		if !credential.ExpiresAt.After(time.Now().UTC()) {
			return ErrCredentialRejected
		}
		if err := tx.db.First(&client, "id = ?", credential.ClientID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCredentialRejected
			}
			return fmt.Errorf("读取客户端失败：%w", translateSQLError(err))
		}
		if client.EnrollmentState == EnrollmentStateRevoked {
			return ErrCredentialRejected
		}

		issued, err := NewClientToken()
		if err != nil {
			return err
		}
		token = issued
		now := time.Now().UTC()

		// 凭据作废与 token 生效同事务。抢占走条件更新：只有把 used_at 从
		// NULL 改成非空的那一次才算成功，避免"读检查通过之后、写入之前"
		// 被第二个请求插手。
		//
		// 注意：当前连接模型（MaxOpenConns=1 + EXCLUSIVE 锁）把事务串行化了，
		// 第二个请求会在上面的读检查处就被拦下，走不到这里。这道条件更新是为
		// 放宽并发模型时准备的防线，因此它是被单独测试覆盖的（见
		// TestMarkCredentialUsedIsConditional），而不是靠并发兑换用例间接覆盖。
		won, err := tx.markCredentialUsed(credential.ID, now)
		if err != nil {
			return err
		}
		if !won {
			return ErrCredentialRejected
		}

		updates := map[string]any{
			"enrollment_state": EnrollmentStateActive,
			"token_digest":     DigestToken(issued),
			"updated_at":       now,
		}
		if err := tx.db.Model(&Client{}).Where("id = ?", client.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("更新客户端失败：%w", translateSQLError(err))
		}
		client.EnrollmentState = EnrollmentStateActive
		client.TokenDigest = DigestToken(issued)
		return tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeClient,
			ActorID:    client.ID,
			Action:     ActionClientRotate,
			ObjectType: ObjectTypeToken,
			ObjectID:   client.ID,
			Result:     AuditResultSuccess,
			Context:    fmt.Sprintf("客户端 %s 完成 enrollment，token 摘要前缀 %s", truncateAuditLabel(client.Name), DigestPrefix(DigestToken(issued))),
		})
	})
	if err != nil {
		return ClientView{}, "", err
	}
	return viewOfClient(client), token, nil
}

// RotateClientToken 轮换客户端 token 并返回新明文。
//
// 新摘要与旧摘要失效在同一事务提交（FR-07 §3），避免出现新旧都不可用的中间态。
// 轮换只影响目标客户端。
func (tx *Tx) RotateClientToken(clientID string) (ClientView, string, error) {
	if clientID == "" {
		return ClientView{}, "", errors.New("客户端标识不能为空")
	}
	var client Client
	var token string

	err := tx.Transaction(func() error {
		if err := tx.db.First(&client, "id = ?", clientID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrClientNotFound
			}
			return fmt.Errorf("读取客户端失败：%w", translateSQLError(err))
		}
		if client.EnrollmentState == EnrollmentStateRevoked {
			return ErrClientNotFound
		}
		issued, err := NewClientToken()
		if err != nil {
			return err
		}
		token = issued
		now := time.Now().UTC()
		// 摘要覆写即旧 token 失效：查询按摘要唯一索引命中，旧摘要不再匹配任何行。
		updates := map[string]any{
			"token_digest": DigestToken(issued),
			"updated_at":   now,
		}
		if err := tx.db.Model(&Client{}).Where("id = ?", client.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("轮换 token 失败：%w", translateSQLError(err))
		}
		client.TokenDigest = DigestToken(issued)
		if err := tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeAdmin,
			ActorID:    "admin",
			Action:     ActionClientRotate,
			ObjectType: ObjectTypeToken,
			ObjectID:   client.ID,
			Result:     AuditResultSuccess,
			Context:    fmt.Sprintf("客户端 %s 的 token 已轮换，新摘要前缀 %s", truncateAuditLabel(client.Name), DigestPrefix(client.TokenDigest)),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ClientView{}, "", err
	}
	return viewOfClient(client), token, nil
}

// RevokeClientToken 吊销客户端 token。
//
// 吊销不可恢复（FR-07 §2）：恢复方式是重新发行凭据并再次 enrollment。吊销同时
// 作废该客户端所有未使用的 enrollment 凭据——留着它们等于留了一条绕过吊销的路径。
func (tx *Tx) RevokeClientToken(clientID string) (ClientView, error) {
	if clientID == "" {
		return ClientView{}, errors.New("客户端标识不能为空")
	}
	var client Client

	err := tx.Transaction(func() error {
		if err := tx.db.First(&client, "id = ?", clientID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrClientNotFound
			}
			return fmt.Errorf("读取客户端失败：%w", translateSQLError(err))
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"enrollment_state": EnrollmentStateRevoked,
			"connection_state": ConnectionStateOffline,
			"updated_at":       now,
		}
		if err := tx.db.Model(&Client{}).Where("id = ?", client.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("吊销 token 失败：%w", translateSQLError(err))
		}
		// 同一事务作废未使用凭据：吊销必须让所有既有凭据一起失效。
		expired := tx.db.Model(&EnrollmentCredential{}).
			Where("client_id = ? AND used_at IS NULL", client.ID).
			Update("used_at", now)
		if expired.Error != nil {
			return fmt.Errorf("作废 enrollment 凭据失败：%w", translateSQLError(expired.Error))
		}
		client.EnrollmentState = EnrollmentStateRevoked
		client.ConnectionState = ConnectionStateOffline
		if err := tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeAdmin,
			ActorID:    "admin",
			Action:     ActionClientRevoke,
			ObjectType: ObjectTypeToken,
			ObjectID:   client.ID,
			Result:     AuditResultSuccess,
			Context:    fmt.Sprintf("客户端 %s 的 token 已吊销，摘要前缀 %s", truncateAuditLabel(client.Name), DigestPrefix(client.TokenDigest)),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ClientView{}, err
	}
	return viewOfClient(client), nil
}

// viewOfClient 把客户端记录转为脱敏视图。
func viewOfClient(client Client) ClientView {
	return ClientView{
		ID:              client.ID,
		Name:            client.Name,
		DigestPrefix:    DigestPrefix(client.TokenDigest),
		EnrollmentState: client.EnrollmentState,
		ConnectionState: client.ConnectionState,
		DesiredRevision: client.DesiredRevision,
		ActiveRevision:  client.ActiveRevision,
	}
}

// markCredentialUsed 把凭据标记为已使用，返回是否由本次调用抢占成功。
//
// 条件是 used_at 仍为 NULL：凭据可能已被另一请求用掉，或在轮换/吊销时被作废。
// 把判断放进 UPDATE 的 WHERE 而不是先读后写，使抢占在数据库层成为一次原子操作。
func (tx *Tx) markCredentialUsed(credentialID string, at time.Time) (bool, error) {
	used := tx.db.Model(&EnrollmentCredential{}).
		Where("id = ? AND used_at IS NULL", credentialID).
		Update("used_at", at)
	if used.Error != nil {
		return false, fmt.Errorf("作废 enrollment 凭据失败：%w", translateSQLError(used.Error))
	}
	return used.RowsAffected > 0, nil
}

// newCredentialID 生成凭据标识。
func newCredentialID() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成凭据标识失败：%w", err)
	}
	return "ec_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}
