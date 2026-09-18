package wire

import "errors"

// ErrorCategory 是 wire 阶段的稳定错误类别。
//
// 拒绝路径必须返回类别而不是自由文案，使上层能按类别判定拒绝原因，
// 不必匹配错误字符串。类别与 docs/specs/wire-v1-v2-codec.md §3.5 判定表对齐。
type ErrorCategory string

const (
	// CategoryVersionNotAccepted 表示该监听入口不接受对端选定的 wire 版本。
	CategoryVersionNotAccepted ErrorCategory = "版本不被接受"
	// CategoryCapabilityMismatch 表示 v2 hello 能力交集为空。
	CategoryCapabilityMismatch ErrorCategory = "能力无法协商"
	// CategoryNegotiationFrameInvalid 表示 hello 帧格式非法。
	CategoryNegotiationFrameInvalid ErrorCategory = "协商帧非法"
	// CategoryLengthExceeded 表示声明长度超过协商或实现上限。
	// 该情形不得按对端声明值分配缓冲区。
	CategoryLengthExceeded ErrorCategory = "长度超限"
	// CategoryLengthInvalid 表示长度字段为负值等不可能取值。
	CategoryLengthInvalid ErrorCategory = "长度非法"
	// CategoryPayloadTruncated 表示载荷未读满即结束。
	CategoryPayloadTruncated ErrorCategory = "载荷截断"
	// CategoryTypeInvalid 表示消息类型缺失、宽度不足或不是已支持类型。
	CategoryTypeInvalid ErrorCategory = "不支持的消息类型"
	// CategoryFrameTypeInvalid 表示 v2 帧类型未定义。
	CategoryFrameTypeInvalid ErrorCategory = "帧类型非法"
	// CategoryFlagsUnsupported 表示 v2 flags 出现非零未知位。
	CategoryFlagsUnsupported ErrorCategory = "flags 不支持"
	// CategoryJSONInvalid 表示载荷不是合法 JSON。
	CategoryJSONInvalid ErrorCategory = "JSON 非法"
	// CategoryTransformRejected 表示加密或压缩状态机的非法迁移。
	CategoryTransformRejected ErrorCategory = "变换迁移非法"
	// CategoryTransportFailure 表示底层连接在 wire 解析期间读取失败。
	// 该类别让传输层异常也能进入统一拒绝出口而不丢失阶段信息。
	CategoryTransportFailure ErrorCategory = "传输读取失败"
)

// Stage 是 wire 阶段标记，用于关闭事件定位失败环节。
type Stage string

const (
	StageDetect    Stage = "版本判定"
	StageNegotiate Stage = "协商"
	StageMessage   Stage = "消息"
)

// ProtocolError 是 wire 层协议错误，携带稳定类别与阶段。
type ProtocolError struct {
	Category ErrorCategory
	Stage    Stage
	Detail   string
}

func (err *ProtocolError) Error() string {
	if err.Detail == "" {
		return string(err.Category)
	}
	return string(err.Category) + "：" + err.Detail
}

// CategoryOf 返回错误的稳定类别；错误不含类别时返回空字符串。
func CategoryOf(err error) ErrorCategory {
	var target *ProtocolError
	if errors.As(err, &target) {
		return target.Category
	}
	return ""
}

// StageOf 返回错误的 wire 阶段；错误不含阶段时返回空字符串。
func StageOf(err error) Stage {
	var target *ProtocolError
	if errors.As(err, &target) {
		return target.Stage
	}
	return ""
}

// IsCategory 判断错误是否属于给定类别。
func IsCategory(err error, category ErrorCategory) bool {
	return CategoryOf(err) == category
}

// protocolError 构造带类别与阶段的协议错误。
func protocolError(category ErrorCategory, stage Stage, detail string) *ProtocolError {
	return &ProtocolError{Category: category, Stage: stage, Detail: detail}
}
