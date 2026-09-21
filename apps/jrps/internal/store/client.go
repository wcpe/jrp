package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

// tokenDigestLength 是 token 明文的最小长度；过短无法提供有效熵。
const tokenRawBytes = 32

// NewClientToken 生成一个客户端 token 明文。
//
// 明文只在创建或轮换响应中出现一次，此后只以摘要形式落库（FR-07 §2）。
func NewClientToken() (string, error) {
	buffer := make([]byte, tokenRawBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成客户端 token 失败：%w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// NewClientID 生成一个客户端标识。
//
// 标识由服务端生成而不是由调用方指定：客户端标识会出现在审计、路由与
// token 归属判定中，允许外部指定会打开伪造他人标识的口子。
func NewClientID() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成客户端标识失败：%w", err)
	}
	return "cli_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// DigestToken 计算 token 摘要。
//
// 摘要使用 SHA-256：它是不可逆的，因此落库内容无法用于直接鉴权（FR-07 §3）。
func DigestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// maxClientNameLength 是客户端名称的字符数上限。
//
// 与通知目标取同一档位，并覆盖审计上下文里最长的固定模板仍有余量。名称上限存在
// 的意义不是"数据库存不下"，而是让超长输入得到 400 与中文说明，而不是在后续
// 某个动作里以 500 的形式爆出来。
const maxClientNameLength = 128

// ErrClientNameInvalid 表示客户端名称不合法。
var ErrClientNameInvalid = errors.New("客户端名称不合法")

// ClientInput 是创建一个客户端所需的输入。
type ClientInput struct {
	ID   string
	Name string
	// Token 是 token 明文；它只在本次调用中出现，落库的只有摘要。
	Token string
}

// CreateClient 写入客户端身份并在同一事务写审计事件。
//
// 落库内容只有 token 摘要；调用方持有明文并只在创建响应中返回一次。
func (tx *Tx) CreateClient(input ClientInput) (uint64, error) {
	if input.ID == "" {
		return 0, errors.New("客户端标识不能为空")
	}
	if input.Token == "" {
		return 0, errors.New("客户端 token 不能为空")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return 0, fmt.Errorf("%w：名称不能为空", ErrClientNameInvalid)
	}
	if utf8.RuneCountInString(name) > maxClientNameLength {
		return 0, fmt.Errorf("%w：名称超出长度上限", ErrClientNameInvalid)
	}
	return 0, tx.Transaction(func() error {
		client := Client{
			ID:              input.ID,
			Name:            name,
			TokenDigest:     DigestToken(input.Token),
			EnrollmentState: EnrollmentStatePending,
			ConnectionState: ConnectionStateOffline,
			CreatedAt:       time.Now().UTC(),
			UpdatedAt:       time.Now().UTC(),
		}
		if err := tx.db.Create(&client).Error; err != nil {
			return fmt.Errorf("写入客户端失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeAdmin,
			ActorID:    "admin",
			Action:     ActionClientCreate,
			ObjectType: "client",
			ObjectID:   client.ID,
			Result:     AuditResultSuccess,
			// 审计只记录标识与摘要短前缀，不记录完整 token。
			Context: fmt.Sprintf("客户端 %s 已创建，token 摘要前缀 %s", truncateAuditLabel(client.Name), DigestPrefix(client.TokenDigest)),
		})
	})
}

// DigestPrefix 返回摘要的短前缀，供日志与审计关联，不足以还原 token。
func DigestPrefix(digest string) string {
	if len(digest) < digestPrefixLength {
		return digest
	}
	return digest[:digestPrefixLength]
}

// digestPrefixLength 是摘要短前缀长度。
const digestPrefixLength = 8

// AuthenticateClientToken 以恒定时间比较摘要完成鉴权，返回客户端记录。
//
// 请求携带的 token 明文不落库、不写日志。
func (tx *Tx) AuthenticateClientToken(token string) (Client, error) {
	if token == "" {
		return Client{}, ErrTokenRejected
	}
	digest := DigestToken(token)
	var client Client
	err := tx.db.First(&client, "token_digest = ?", digest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 恒定时间比较一个固定长度的假摘要，避免通过耗时差异区分"token 不存在"与"token 错误"。
		subtle.ConstantTimeCompare([]byte(digest), []byte(digest))
		return Client{}, ErrTokenRejected
	}
	if err != nil {
		return Client{}, fmt.Errorf("鉴权查询失败：%w", translateSQLError(err))
	}
	if subtle.ConstantTimeCompare([]byte(client.TokenDigest), []byte(digest)) != 1 {
		return Client{}, ErrTokenRejected
	}
	if client.EnrollmentState == EnrollmentStateRevoked {
		return Client{}, ErrTokenRejected
	}
	return client, nil
}

// ErrTokenRejected 表示 token 无效或已失效；对外不区分具体原因。
var ErrTokenRejected = errors.New("token 无效或已失效")

// ClientView 是客户端的读取视图；它不含 token 明文，也不含完整摘要。
type ClientView struct {
	ID              string
	Name            string
	DigestPrefix    string
	EnrollmentState string
	ConnectionState string
	DesiredRevision uint64
	// ActiveRevision 是最近一次成功 publish 的 Apply 结果记录，仅供展示（ADR-0012）。
	ActiveRevision uint64
}

// MaskedToken 返回可安全展示的 token 掩码。
func (v ClientView) MaskedToken() string {
	if v.DigestPrefix == "" {
		return "(未设置)"
	}
	return "****" + v.DigestPrefix
}

// Client 读取客户端的脱敏视图；完整 token 明文不以任何形式返回。
func (tx *Tx) Client(id string) (ClientView, error) {
	var client Client
	err := tx.db.First(&client, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 返回哨兵错误而不是普通错误：HTTP 层据此把"客户端不存在"映射为 404。
		// 用普通错误时该分支不可达，不存在的客户端会被报成服务内部错误。
		return ClientView{}, ErrClientNotFound
	}
	if err != nil {
		return ClientView{}, fmt.Errorf("读取客户端失败：%w", translateSQLError(err))
	}
	return ClientView{
		ID:              client.ID,
		Name:            client.Name,
		DigestPrefix:    DigestPrefix(client.TokenDigest),
		EnrollmentState: client.EnrollmentState,
		ConnectionState: client.ConnectionState,
		DesiredRevision: client.DesiredRevision,
		ActiveRevision:  client.ActiveRevision,
	}, nil
}

// Clients 返回全部客户端的脱敏视图。
func (tx *Tx) Clients() ([]ClientView, error) {
	var clients []Client
	if err := tx.db.Order("id ASC").Find(&clients).Error; err != nil {
		return nil, fmt.Errorf("读取客户端列表失败：%w", translateSQLError(err))
	}
	views := make([]ClientView, 0, len(clients))
	for _, client := range clients {
		views = append(views, ClientView{
			ID:              client.ID,
			Name:            client.Name,
			DigestPrefix:    DigestPrefix(client.TokenDigest),
			EnrollmentState: client.EnrollmentState,
			ConnectionState: client.ConnectionState,
			DesiredRevision: client.DesiredRevision,
			ActiveRevision:  client.ActiveRevision,
		})
	}
	return views, nil
}
