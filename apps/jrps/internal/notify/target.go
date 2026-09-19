package notify

import (
	"time"
)

// SMTP 安全传输选项。
//
// notify 包自带一份取值而不引用 store：渠道实现不依赖存储层，两者的
// 转换由调用方完成（见 Target 的说明）。取值必须与 store 包保持一致。
const (
	// SMTPSecurityNone 是明文 SMTP，仅允许显式选择：默认值不是它。
	SMTPSecurityNone = "none"
	// SMTPSecurityStartTLS 是先明文连接再升级 TLS，P1 的默认与推荐取值。
	SMTPSecurityStartTLS = "starttls"
)

// Target 是一次投递所需的目标配置。
//
// 与 store 的持久化模型分开定义：notify 只关心"投递需要哪些字段"，
// 不依赖存储层；两者的转换由调用方完成，避免渠道实现被存储结构反向绑定。
// 转换时必须只填充投递所需字段，不把无关的库内字段带进来。
type Target struct {
	ID   string
	Type string
	Name string

	// Secret 是 Webhook 签名密钥或 SMTP 密码；只用于建立请求，
	// 不得出现在日志、错误信息或载荷中。
	Secret string

	// WebhookURL 仅在 Type 为 webhook 时使用。
	WebhookURL string

	// SMTP 配置仅在 Type 为 email 时使用。
	SMTPHost     string
	SMTPPort     int
	SMTPFrom     string
	SMTPTo       []string
	SMTPSecurity string
}

// Notification 是一条待投递的通知。
//
// Payload 已由写入方脱敏：本层不解析其内容，也不做二次过滤——脱敏责任在
// 产生通知的业务侧，渠道层只管搬运。这样划分是因为渠道层无法判断某个
// 字符串是否是秘密，只有产生它的一方知道。
type Notification struct {
	EventID    string
	EventType  string
	OccurredAt time.Time
	Payload    string
}
