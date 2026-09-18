package core

import (
	"errors"
	"strconv"
)

// ErrConfigInvalid 是配置校验失败的哨兵错误，宿主用 errors.Is(err, ErrConfigInvalid) 判定。
//
// 字符串匹配不构成稳定契约，宿主必须通过 ConfigError 的错误码判断失败类别。
var ErrConfigInvalid = errors.New("配置校验未通过")

// ErrorCode 是配置问题的稳定机器可读分类。
type ErrorCode string

const (
	// CodeIncomplete 表示必填字段缺失，例如未提供服务端端点、监听端点或客户端凭证。
	CodeIncomplete ErrorCode = "incomplete"
	// CodePortOutOfRange 表示端口为 0、负数或大于 65535。
	CodePortOutOfRange ErrorCode = "port_out_of_range"
	// CodeMissingAuth 表示缺少鉴权信息，例如未设置鉴权材料或标识、token 为空。
	CodeMissingAuth ErrorCode = "missing_auth"
	// CodeDuplicateProxyName 表示同一配置内出现重名代理。
	CodeDuplicateProxyName ErrorCode = "duplicate_proxy_name"
	// CodeInvalidAddress 表示地址未指定 IP 或不满足场景要求。
	CodeInvalidAddress ErrorCode = "invalid_address"
	// CodeUnsupportedValue 表示传输、wire 版本或代理类型取值不受当前已交付能力支持。
	CodeUnsupportedValue ErrorCode = "unsupported_value"
	// CodeUnknownClient 表示服务端代理绑定引用了不存在的客户端标识。
	CodeUnknownClient ErrorCode = "unknown_client"
	// CodePortConflict 表示入口端口已被其它代理独占，对应注册校验的冲突环节。
	CodePortConflict ErrorCode = "port_conflict"
	// CodeRouteConflict 表示同一入口端口上主机名与路径组合重复。
	CodeRouteConflict ErrorCode = "route_conflict"
	// CodeInvalidRoute 表示 HTTP 路由项的主机名或路径前缀非法。
	CodeInvalidRoute ErrorCode = "invalid_route"
	// CodeInvalidDuration 表示心跳或超时为负值。
	CodeInvalidDuration ErrorCode = "invalid_duration"
	// CodeLimitExceeded 表示条目数量或名称长度超出 Core 常量上限。
	CodeLimitExceeded ErrorCode = "limit_exceeded"
)

// ConfigError 描述一条校验问题：稳定的错误码、字段路径与可公开的中文消息。
//
// 消息只说明缺失或非法的字段，不回显 token、密码等凭证原文。
type ConfigError struct {
	code    ErrorCode
	field   string
	message string
}

// Code 返回问题的稳定错误码。
func (configError *ConfigError) Code() ErrorCode {
	return configError.code
}

// Field 返回问题的稳定字段路径，例如 proxies[1].remotePort。
func (configError *ConfigError) Field() string {
	return configError.field
}

// Message 返回可安全公开的中文消息，不含凭证原文。
func (configError *ConfigError) Message() string {
	return configError.message
}

// Error 返回包含字段路径与消息的可读表达。
func (configError *ConfigError) Error() string {
	return string(configError.code) + " " + configError.field + "：" + configError.message
}

// Unwrap 返回哨兵 ErrConfigInvalid，使 errors.Is 可判定。
func (configError *ConfigError) Unwrap() error {
	return ErrConfigInvalid
}

// ConfigErrors 聚合一次构建或校验中发现全部问题，按校验顺序排列。
//
// 宿主可用 errors.As(err, &problems) 取出完整列表，一次修完所有问题。
type ConfigErrors []*ConfigError

// Error 返回逐条分行的问题清单。
func (problems ConfigErrors) Error() string {
	if len(problems) == 0 {
		return ErrConfigInvalid.Error()
	}

	message := ErrConfigInvalid.Error() + "，共 " + strconv.Itoa(len(problems)) + " 处问题："
	for _, problem := range problems {
		message += "\n- " + problem.Error()
	}
	return message
}

// Unwrap 返回全部问题，使 errors.Is 与 errors.As 能遍历每一条 ConfigError。
func (problems ConfigErrors) Unwrap() []error {
	unwrapped := make([]error, 0, len(problems))
	for _, problem := range problems {
		unwrapped = append(unwrapped, problem)
	}
	return unwrapped
}

// newConfigError 由错误码、字段路径与中文说明构造一条问题。
func newConfigError(code ErrorCode, field, message string) *ConfigError {
	return &ConfigError{code: code, field: field, message: message}
}
