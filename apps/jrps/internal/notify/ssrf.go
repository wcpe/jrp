package notify

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 出站目标校验的稳定错误说明。
var (
	// ErrTargetBlocked 表示目标地址落在禁止范围内。
	ErrTargetBlocked = errors.New("通知目标地址被拒绝")
	// ErrTargetMalformed 表示目标地址格式不合法。
	ErrTargetMalformed = errors.New("通知目标地址格式不合法")
)

// 允许的出站协议；P1 只允许 HTTPS，避免明文传输通知内容与签名。
var allowedSchemes = map[string]struct{}{
	"https": {},
}

// 允许的出站端口。这是显式白名单而非"除禁止端口外都允许"：
// 白名单让规则可穷举，不会因遗漏某个内部服务端口而留下缺口。
var allowedPorts = map[int]struct{}{
	443:  {},
	8443: {},
	9443: {},
}

// maxURLLength 限制目标地址长度，避免超长输入进入校验与日志。
const maxURLLength = 512

// blockedPrefixes 是禁止出站访问的地址段。
//
// 覆盖规格 §3.4 列举的三类：回环、链路本地与私有地址。额外覆盖未指定地址、
// 组播与保留段——它们都不是通知目标的合理取值，放行只会扩大攻击面。
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // 未指定
	netip.MustParsePrefix("10.0.0.0/8"),      // 私有
	netip.MustParsePrefix("100.64.0.0/10"),   // 运营商级 NAT
	netip.MustParsePrefix("127.0.0.0/8"),     // 回环
	netip.MustParsePrefix("169.254.0.0/16"),  // 链路本地
	netip.MustParsePrefix("172.16.0.0/12"),   // 私有
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF 协议分配
	netip.MustParsePrefix("192.0.2.0/24"),    // 文档用
	netip.MustParsePrefix("192.168.0.0/16"),  // 私有
	netip.MustParsePrefix("198.18.0.0/15"),   // 基准测试
	netip.MustParsePrefix("198.51.100.0/24"), // 文档用
	netip.MustParsePrefix("203.0.113.0/24"),  // 文档用
	netip.MustParsePrefix("224.0.0.0/4"),     // 组播
	netip.MustParsePrefix("240.0.0.0/4"),     // 保留
	netip.MustParsePrefix("::1/128"),         // IPv6 回环
	netip.MustParsePrefix("::/128"),          // IPv6 未指定
	netip.MustParsePrefix("fc00::/7"),        // IPv6 唯一本地
	netip.MustParsePrefix("fe80::/10"),       // IPv6 链路本地
	netip.MustParsePrefix("ff00::/8"),        // IPv6 组播
	netip.MustParsePrefix("64:ff9b::/96"),    // IPv6 到 IPv4 转换
	netip.MustParsePrefix("::ffff:0:0/96"),   // IPv4 映射，需按内嵌地址判定
}

// validateWebhookURL 是 Send 使用的入口：skipAddressCheck 仅供测试放开回环限制。
//
// 生产路径恒传 false。放开时仍校验协议、主机与端口形态，只跳过地址范围判定，
// 使测试能连到 httptest 的回环服务器而不必关闭全部校验。
func validateWebhookURL(rawURL string, skipAddressCheck bool) (*url.URL, error) {
	if !skipAddressCheck {
		return ValidateWebhookURL(rawURL)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w：无法解析", ErrTargetMalformed)
	}
	if parsed.Scheme == "" {
		return nil, fmt.Errorf("%w：缺少协议", ErrTargetMalformed)
	}
	if _, ok := allowedSchemes[parsed.Scheme]; !ok {
		return nil, fmt.Errorf("%w：只允许 HTTPS 协议", ErrTargetBlocked)
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("%w：缺少主机名", ErrTargetMalformed)
	}
	return parsed, nil
}

// ValidateWebhookURL 校验 Webhook 目标地址。
//
// 校验发生在发送之前且每次发送都做：域名解析结果可能随时间变化，只在创建
// 目标时校验一次会让"当初合法、后来指向内网"的目标继续可用（DNS 重绑定）。
// 因此本函数只做地址形态校验，解析后的地址检查在 DialContext 中逐次进行。
func ValidateWebhookURL(rawURL string) (*url.URL, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("%w：地址不能为空", ErrTargetMalformed)
	}
	if len(rawURL) > maxURLLength {
		return nil, fmt.Errorf("%w：地址超长", ErrTargetMalformed)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w：无法解析", ErrTargetMalformed)
	}
	// 没有协议是格式问题，与"协议不被允许"是两回事：前者是调用方写错了地址，
	// 后者是地址本身合法但违反安全策略。区分二者让调用方能给出准确提示。
	if parsed.Scheme == "" {
		return nil, fmt.Errorf("%w：缺少协议", ErrTargetMalformed)
	}
	if _, ok := allowedSchemes[parsed.Scheme]; !ok {
		return nil, fmt.Errorf("%w：只允许 HTTPS 协议", ErrTargetBlocked)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w：缺少主机名", ErrTargetMalformed)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w：地址不得内嵌凭据", ErrTargetBlocked)
	}
	if err := validatePort(parsed); err != nil {
		return nil, err
	}
	// 字面量地址当场判定；域名留待拨号时按解析结果判定。
	if literal, err := netip.ParseAddr(host); err == nil {
		if err := checkAddressAllowed(literal); err != nil {
			return nil, err
		}
	}
	return parsed, nil
}

// validatePort 校验端口属于白名单。
func validatePort(parsed *url.URL) error {
	portText := parsed.Port()
	if portText == "" {
		// 未显式写端口时取协议默认端口。
		if parsed.Scheme == "https" {
			return nil
		}
		return fmt.Errorf("%w：缺少端口", ErrTargetMalformed)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("%w：端口不是数字", ErrTargetMalformed)
	}
	if _, ok := allowedPorts[port]; !ok {
		return fmt.Errorf("%w：端口 %d 不在允许范围内", ErrTargetBlocked, port)
	}
	return nil
}

// checkAddressAllowed 判定单个地址是否落在禁止范围内。
//
// 比对前必须剥离 zone：`Unmap()` 只处理 IPv4 映射，**不剥离 zone**，而
// `netip.Prefix.Contains` 对带 zone 的地址一律返回 false。若直接用
// `Unmap()` 的结果比对，`[::1%25lo]` 这类地址会绕过全部禁止段——实测确认
// `[::1%25lo]`、`[fe80::1%25eth0]`、`[fd00::1%25eth0]` 在只做 Unmap 时全部放行。
// zone 只是本机接口限定符，剥掉它不影响地址本身的归属判定。
func checkAddressAllowed(address netip.Addr) error {
	candidate := address.WithZone("").Unmap()
	if !candidate.IsValid() {
		return fmt.Errorf("%w：地址无效", ErrTargetMalformed)
	}
	// 剥离 zone 后重新确认：带 zone 的地址在剥离前 IsValid 为真，但若剥离过程
	// 出现异常形态，这里必须明确拒绝而不是按"未知"放行。
	if candidate.Zone() != "" {
		return fmt.Errorf("%w：地址形态不合法", ErrTargetMalformed)
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(candidate) {
			return fmt.Errorf("%w：禁止指向 %s 范围", ErrTargetBlocked, prefix)
		}
	}
	return nil
}

// CheckResolvedAddresses 校验域名解析出的全部地址。
//
// 全部地址都要检查而不是只查第一个：一个域名可以同时解析出公网与内网地址，
// 只查首个会让拨号实际选中内网地址（DNS 重绑定与轮询解析的常见形态）。
func CheckResolvedAddresses(addresses []net.IP) error {
	if len(addresses) == 0 {
		return fmt.Errorf("%w：域名未解析出任何地址", ErrTargetMalformed)
	}
	for _, address := range addresses {
		parsed, ok := netip.AddrFromSlice(address)
		if !ok {
			return fmt.Errorf("%w：解析结果无效", ErrTargetMalformed)
		}
		if err := checkAddressAllowed(parsed); err != nil {
			return err
		}
	}
	return nil
}

// outboundControl 在拨号建立前校验实际连接的地址。
//
// 这是 SSRF 防线的关键一环：仅校验域名解析结果仍会被"解析后目标变更"
// 绕过，而在 Dialer.Control 层校验的是内核即将连接的真实地址。
// 签名必须匹配 net.Dialer.Control（network、address、syscall.RawConn）。
func outboundControl(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w：拨号地址无法解析", ErrTargetMalformed)
	}
	candidate, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w：拨号地址不是 IP", ErrTargetMalformed)
	}
	return checkAddressAllowed(candidate)
}

// outboundDialerTimeout 是建立出站连接的超时上限。
const outboundDialerTimeout = 10 * time.Second

// newOutboundDialer 构造带地址校验的出站拨号器。
//
// 所有出站渠道都必须经由它建立连接：把关口收在一处，才能保证没有哪条
// 渠道绕开 SSRF 校验。
func newOutboundDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   outboundDialerTimeout,
		Control:   outboundControl,
		KeepAlive: -1, // 通知场景不需要 TCP keepalive
	}
}
