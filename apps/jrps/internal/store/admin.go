package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

// P1 只有一个管理员，不使用角色或租户字段（FR-02 规格 §2）。
const AdminUsername = "admin"

// 密码派生参数：迭代次数与密钥长度随记录保存，便于将来升级而不影响已落库记录。
const (
	passwordMinLength  = 12
	passwordMaxLength  = 256
	passwordSaltBytes  = 16
	passwordKeyBytes   = 32
	passwordIterations = 600000
	passwordKDFName    = "pbkdf2-sha256"
)

// defaultSessionTTL 是会话有效期：空闲过期与绝对过期共用同一超时（FR-02 规格 §6 待定项）。
const defaultSessionTTL = 12 * time.Hour

// 会话令牌与 CSRF 令牌的随机字节数。
const (
	sessionTokenRawBytes = 32
	csrfTokenRawBytes    = 32
)

// ErrAdminAlreadyInitialized 表示管理凭据已存在，初始化路径已永久关闭。
var ErrAdminAlreadyInitialized = errors.New("管理员已完成初始化，不得重复执行 jrps init")

// ErrInvalidCredentials 表示凭据不正确；对外不区分用户名与密码错误。
var ErrInvalidCredentials = errors.New("用户名或密码错误")

// ErrSessionInvalid 表示会话不存在、已过期或已注销。
var ErrSessionInvalid = errors.New("会话无效或已过期")

// randomToken 生成一个 URL 安全的随机令牌明文。
func randomToken(rawBytes int) (string, error) {
	buffer := make([]byte, rawBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成随机令牌失败：%w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// InitializeAdminInput 是初始化唯一管理员所需的输入。
type InitializeAdminInput struct {
	// Password 是管理员密码明文；它只在本次调用中出现，派生后立即丢弃。
	Password string
	// Now 允许测试注入时间；零值使用当前时间。
	Now time.Time
}

// InitializeAdmin 在同一事务写入管理员记录与初始化完成标记。
//
// 该路径一次性：已存在管理员记录时必须退回 ErrAdminAlreadyInitialized，
// 既不覆盖密码也不重置既有会话安全策略（FR-02 规格 §2.1）。
func (tx *Tx) InitializeAdmin(input InitializeAdminInput) error {
	if err := validatePassword(input.Password); err != nil {
		return err
	}
	return tx.Transaction(func() error {
		initialized, err := tx.IsInitialized()
		if err != nil {
			return err
		}
		if initialized {
			return ErrAdminAlreadyInitialized
		}
		record, err := newAdminCredential(input)
		if err != nil {
			return err
		}
		if err := tx.db.Create(record).Error; err != nil {
			return fmt.Errorf("写入管理员记录失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeAdmin,
			ActorID:    AdminUsername,
			Action:     ActionAdminInitialized,
			ObjectType: "admin_credential",
			ObjectID:   AdminUsername,
			Result:     AuditResultSuccess,
			Context:    "管理员初始化完成，初始化路径已永久关闭",
		})
	})
}

// validatePassword 在边界校验密码长度与字符集，超限一律给出中文提示。
func validatePassword(password string) error {
	if password == "" {
		return errors.New("管理员密码不能为空")
	}
	if len(password) < passwordMinLength {
		return fmt.Errorf("管理员密码长度不足：至少需要 %d 个字符", passwordMinLength)
	}
	if len(password) > passwordMaxLength {
		return fmt.Errorf("管理员密码超长：最多允许 %d 个字符", passwordMaxLength)
	}
	if !utf8.ValidString(password) {
		return errors.New("管理员密码必须是有效的 UTF-8 文本")
	}
	return nil
}

// newAdminCredential 派生管理员凭据材料。
//
// 派生使用 PBKDF2-HMAC-SHA256：它是标准库能力，不需要引入新的第三方依赖。
// 派生参数与盐随记录保存，落库材料只是派生结果，不构成可重放的明文密码。
func newAdminCredential(input InitializeAdminInput) (*AdminCredential, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("生成管理员密码盐失败：%w", err)
	}
	initializedAt := input.Now
	if initializedAt.IsZero() {
		initializedAt = time.Now().UTC()
	}
	return &AdminCredential{
		Username:       AdminUsername,
		PasswordDigest: derivePassword(passwordKDFName, []byte(input.Password), salt),
		PasswordSalt:   base64.RawStdEncoding.EncodeToString(salt),
		PasswordParams: fmt.Sprintf("%s;i=%d;dklen=%d",
			passwordKDFName, passwordIterations, passwordKeyBytes),
		InitializedAt:    initializedAt,
		InitializationID: deriveInitializationID(salt, initializedAt),
		CreatedAt:        initializedAt,
		UpdatedAt:        initializedAt,
	}, nil
}

// derivePassword 计算密码派生材料；结果是十六进制字符串，不含密码明文。
func derivePassword(kdf string, password, salt []byte) string {
	iterations := passwordIterations
	length := passwordKeyBytes
	key, err := pbkdf2.Key(sha256.New, string(password), salt, iterations, length)
	if err != nil {
		// pbkdf2.Key 只对非正的迭代次数或负密钥长度报错，两者都由本文件的常量保证。
		panic(fmt.Sprintf("派生管理员密码材料失败：%v", err))
	}
	return hex.EncodeToString(key)
}

// deriveInitializationID 生成初始化标记，与管理员记录同事务提交。
func deriveInitializationID(salt []byte, initializedAt time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", hex.EncodeToString(salt), initializedAt.UnixNano())))
	return hex.EncodeToString(sum[:])
}

// IsInitialized 判断初始化是否已完成：存在管理员记录即视为已完成。
func (tx *Tx) IsInitialized() (bool, error) {
	var count int64
	err := tx.db.Model(&AdminCredential{}).Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("读取管理员记录失败：%w", translateSQLError(err))
	}
	return count > 0, nil
}

// AuthenticateAdmin 校验管理员凭据并返回其记录。
//
// 失败时对"用户名不存在"与"密码错误"返回同一个哨兵错误，避免账号枚举（FR-02 规格 §2.2）。
func (tx *Tx) AuthenticateAdmin(username, password string) (AdminCredential, error) {
	if username == "" || password == "" {
		return AdminCredential{}, ErrInvalidCredentials
	}
	// 长度上限在查询之前判定：超长输入不可能命中任何真实凭据，却会一路走到
	// 派生与审计写入，让未认证请求用超大载荷推高响应体积与库内记录。
	// 上限取审计标识的同一口径，保证合法用户名在任何路径上都不会被截断。
	if utf8.RuneCountInString(username) > maxAuditIdentifierRunes ||
		len(password) > passwordMaxLength {
		return AdminCredential{}, ErrInvalidCredentials
	}
	var credential AdminCredential
	err := tx.db.First(&credential, "username = ?", username).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AdminCredential{}, ErrInvalidCredentials
	}
	if err != nil {
		return AdminCredential{}, fmt.Errorf("读取管理员记录失败：%w", translateSQLError(err))
	}
	if !verifyAdminPassword(credential, password) {
		return AdminCredential{}, ErrInvalidCredentials
	}
	return credential, nil
}

// verifyAdminPassword 以恒定时间比较派生结果，避免通过耗时差异推测密码。
func verifyAdminPassword(credential AdminCredential, password string) bool {
	salt, err := base64.RawStdEncoding.DecodeString(credential.PasswordSalt)
	if err != nil {
		return false
	}
	expected := []byte(derivePassword(passwordKDFName, []byte(password), salt))
	actual := []byte(credential.PasswordDigest)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// RecordLoginFailure 记录一次登录失败审计事件；审计不写密码或凭据材料。
func (tx *Tx) RecordLoginFailure(username string) error {
	return tx.writeAudit(AuditEvent{
		ActorType:  ActorTypeAdmin,
		ActorID:    username,
		Action:     ActionAdminLoginFailure,
		ObjectType: "admin_credential",
		ObjectID:   username,
		Result:     AuditResultDenied,
		Context:    "管理员登录失败：用户名或密码错误",
	})
}

// SessionToken 是新建会话时一次性返回的令牌材料。
//
// Token 与 CSRFToken 只在建立响应中出现一次：服务端只保留 Token 的摘要，
// 不保存可重放的会话密钥明文（FR-09 规格 §3.3）。
type SessionToken struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
}

// CreateSessionInput 是建立会话所需的输入。
type CreateSessionInput struct {
	// Username 是会话归属的管理员名，用于审计主体。
	Username string
	// TTL 是会话有效期；零值使用默认有效期。
	TTL time.Duration
	// Now 允许测试注入时间；零值使用当前时间。
	Now time.Time
}

// CreateSession 建立会话并在同一事务写入登录审计事件。
//
// 落库的只有会话令牌摘要：服务端不保留可重放的会话密钥明文（FR-09 规格 §3.3）。
// CSRF 状态是必须回显给浏览器脚本的防跨站值，不属于会话密钥，因此随会话保存。
func (tx *Tx) CreateSession(input CreateSessionInput) (SessionToken, error) {
	token, err := randomToken(sessionTokenRawBytes)
	if err != nil {
		return SessionToken{}, err
	}
	csrfToken, err := randomToken(csrfTokenRawBytes)
	if err != nil {
		return SessionToken{}, err
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ttl := input.TTL
	if ttl == 0 {
		ttl = defaultSessionTTL
	}
	issued := SessionToken{Token: token, CSRFToken: csrfToken, ExpiresAt: now.Add(ttl)}
	err = tx.Transaction(func() error {
		record := Session{
			TokenDigest: DigestToken(token),
			// CSRF 状态是防跨站值而非会话密钥：它必须回显给浏览器脚本才能在后续
			// 修改请求中携带，因此以明文随会话保存；会话令牌本身仍只以摘要落库。
			CSRFToken: csrfToken,
			CreatedAt: now,
			ExpiresAt: issued.ExpiresAt,
		}
		if err := tx.db.Create(&record).Error; err != nil {
			return fmt.Errorf("写入会话记录失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeAdmin,
			ActorID:    input.Username,
			Action:     ActionAdminLogin,
			ObjectType: "session",
			ObjectID:   DigestPrefix(record.TokenDigest),
			Result:     AuditResultSuccess,
			Context:    "管理员登录成功，会话已建立",
		})
	})
	if err != nil {
		return SessionToken{}, err
	}
	return issued, nil
}

// ActiveSession 返回仍然有效的会话；不存在、已过期或已注销一律返回 ErrSessionInvalid。
func (tx *Tx) ActiveSession(token string) (Session, error) {
	if token == "" {
		return Session{}, ErrSessionInvalid
	}
	digest := DigestToken(token)
	var record Session
	err := tx.db.First(&record, "token_digest = ?", digest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Session{}, ErrSessionInvalid
	}
	if err != nil {
		return Session{}, fmt.Errorf("读取会话失败：%w", translateSQLError(err))
	}
	if record.InvalidatedAt != nil {
		return Session{}, ErrSessionInvalid
	}
	if !record.ExpiresAt.After(time.Now().UTC()) {
		return Session{}, ErrSessionInvalid
	}
	if subtle.ConstantTimeCompare([]byte(record.TokenDigest), []byte(digest)) != 1 {
		return Session{}, ErrSessionInvalid
	}
	return record, nil
}

// VerifySessionCSRF 校验会话的 CSRF 状态是否匹配；不匹配返回 ErrSessionInvalid。
func (tx *Tx) VerifySessionCSRF(token, csrfToken string) bool {
	if token == "" || csrfToken == "" {
		return false
	}
	record, err := tx.ActiveSession(token)
	if err != nil {
		return false
	}
	// CSRF 状态以明文随会话保存，比较时同样使用明文并以恒定时间完成，
	// 避免通过耗时差异枚举防跨站值。
	expected := []byte(record.CSRFToken)
	actual := []byte(csrfToken)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// InvalidateSession 注销会话并在同一事务写入登出审计事件；注销后会话立即失效。
func (tx *Tx) InvalidateSession(token string) error {
	if token == "" {
		return ErrSessionInvalid
	}
	return tx.Transaction(func() error {
		record, err := tx.ActiveSession(token)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		err = tx.db.Model(&Session{}).
			Where("id = ? AND invalidated_at IS NULL", record.ID).
			Update("invalidated_at", now).Error
		if err != nil {
			return fmt.Errorf("注销会话失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			ActorType:  ActorTypeAdmin,
			ActorID:    AdminUsername,
			Action:     ActionAdminLogout,
			ObjectType: "session",
			ObjectID:   DigestPrefix(record.TokenDigest),
			Result:     AuditResultSuccess,
			Context:    "管理员已登出，会话标识立即失效",
		})
	})
}
