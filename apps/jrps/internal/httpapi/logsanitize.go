package httpapi

import (
	"net/url"
	"strings"
)

// 本文件是 FR-12 规格 §3.4 脱敏矩阵的统一实现：所有日志与摘要输出共用，
// 不允许各生产点各写一套——脱敏口径漂移就是泄露。
//
// 测试以「输出不含敏感串」断言（规格 §3.4），而不是以「调用了脱敏函数」断言。

// RedactToken 把客户端 token 掩码为可关联的摘要前缀。
//
// 规格 §3.4：完整值禁止出现，只允许摘要前缀与客户端标识。保留前缀是为了让
// 管理员能把日志行与某个 token 对上，同时不暴露可复用的凭据材料。
func RedactToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "(空)"
	}
	if len(token) <= 6 {
		return "***"
	}
	return token[:6] + "…"
}

// RedactSecret 把密码与派生材料完全掩码（规格 §3.4：禁止包括长度、前缀与
// 哈希片段在内的任何可推断信息）。
func RedactSecret(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return "(空)"
	}
	return "(已隐藏)"
}

// RedactCookieValue 掩码 Cookie 值；名称与存在性标记由调用方自行保留。
func RedactCookieValue(name, value string) string {
	_ = name
	if strings.TrimSpace(value) == "" {
		return "(空)"
	}
	return "(已隐藏)"
}

// RedactAuthorization 掩码 Authorization 头：只保留方案类别（bearer、basic 等）。
func RedactAuthorization(header string) string {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return "(空)"
	}
	scheme, _, found := strings.Cut(trimmed, " ")
	if !found {
		// 无方案的原始凭据：整个头部都是敏感材料，一律隐藏。
		return "(已隐藏)"
	}
	return strings.ToLower(scheme) + " (已隐藏)"
}

// SummaryPath 生成请求路径摘要：保留路径与查询串键名，掩码查询串值
// （规格 §3.4：查询串中的键值视为敏感，按名称掩码）。
//
// 解析失败的原始串按整条敏感处理，只保留「(不可解析)」占位。
func SummaryPath(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	parsed, err := url.Parse(rawPath)
	if err != nil {
		return "(不可解析)"
	}
	summary := parsed.Path
	if summary == "" {
		summary = "/"
	}
	query := parsed.Query()
	if len(query) == 0 {
		return summary
	}
	pairs := make([]string, 0, len(query))
	for key := range query {
		pairs = append(pairs, key+"=(已隐藏)")
	}
	return summary + "?" + strings.Join(pairs, "&")
}

// AddrSummary 生成远端地址摘要：保留地址与端口（FR-12 规格 §3.4：只记录
// 地址摘要层级，不携带可反推凭证的字段）。当前实现原样返回——远端地址里
// 没有凭据材料；保留函数作为显式脱敏点，后续收紧时只改这里。
func AddrSummary(address string) string {
	return strings.TrimSpace(address)
}
