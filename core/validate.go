package core

import (
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// maxPort 是合法端口的闭区间上界；端口 0 表示自动分配，本配置不支持。
const maxPort = 65535

// Validate 校验客户端配置并返回聚合错误；无副作用，可重复调用。
//
// 校验按固定顺序执行并收集全部问题：端点完整性与字段、鉴权字段、代理条目、数量上限、时间参数。
func (config ClientConfig) Validate() error {
	problems := validateClientEndpoint(config)
	problems = append(problems, validateClientAuth(config)...)
	problems = append(problems, validateClientProxies(config.proxies)...)
	problems = append(problems, validateLimit("proxies", len(config.proxies), MaxProxyCount)...)
	problems = append(problems, validateDurations(config.heartbeat, config.timeout, config.drainTimeout)...)
	problems = append(problems, validatePoolLimits(config.poolSize, config.idleLimit)...)
	return collectProblems(problems)
}

// Validate 校验服务端配置并返回聚合错误；无副作用，可重复调用。
//
// 校验按固定顺序执行并收集全部问题：监听与 wire 字段、凭证集合、代理绑定集合、数量上限、时间参数。
func (config ServerConfig) Validate() error {
	problems := validateListen(config)
	problems = append(problems, validateWire(config.wire)...)
	problems = append(problems, validateCredentials(config.credentials)...)
	problems = append(problems, validateLimit("credentials", len(config.credentials), MaxClientCredentialCount)...)
	problems = append(problems, validateBindings(config.bindings, config.credentials)...)
	problems = append(problems, validateLimit("bindings", len(config.bindings), MaxProxyCount)...)
	problems = append(problems, validateDurations(config.heartbeat, config.timeout, config.drainTimeout)...)
	problems = append(problems, validateServerIdleLimit(config.idleLimit)...)
	return collectProblems(problems)
}

// validateClientEndpoint 校验服务端端点与其中携带的传输与 wire 取值。
func validateClientEndpoint(config ClientConfig) []*ConfigError {
	if !config.hasEndpoint {
		return incomplete("endpoint", "必须提供服务端端点")
	}

	problems := validateTargetAddress("endpoint.address", config.endpoint.Address)
	problems = append(problems, validateEnum("endpoint.transport", config.endpoint.Transport, supportedTransports)...)
	problems = append(problems, validateEnum("endpoint.wire", config.endpoint.Wire, supportedWireVersions)...)
	return problems
}

// validateClientAuth 校验客户端标识与鉴权材料；消息只说明缺失字段，不回显 token。
func validateClientAuth(config ClientConfig) []*ConfigError {
	problems := make([]*ConfigError, 0, 2)
	if config.clientID == "" {
		problems = append(problems, newConfigError(CodeMissingAuth, "clientID", "必须提供客户端标识"))
	}
	if config.auth.Token == "" {
		problems = append(problems, newConfigError(CodeMissingAuth, "auth.token", "必须提供鉴权 token"))
	}
	return problems
}

// validateClientProxies 逐条校验本地代理，并在最后检测重名。
func validateClientProxies(proxies []TCPProxy) []*ConfigError {
	problems := make([]*ConfigError, 0, len(proxies))
	for index, proxy := range proxies {
		field := itemField("proxies", index)
		problems = append(problems, validateName(field+"name", proxy.Name)...)
		problems = append(problems, validateTargetAddress(field+"localAddr", proxy.LocalAddr)...)
		problems = append(problems, validatePort(field+"remotePort", proxy.RemotePort)...)
	}
	return append(problems, duplicateNameProblems("proxies", proxies, func(proxy TCPProxy) string {
		return proxy.Name
	})...)
}

// validateListen 校验监听端点。
func validateListen(config ServerConfig) []*ConfigError {
	if !config.hasListen {
		return incomplete("listen", "必须提供监听端点")
	}

	problems := validateListenAddress("listen.address", config.listen.Address)
	problems = append(problems, validateEnum("listen.transport", config.listen.Transport, supportedTransports)...)
	return problems
}

// validateWire 校验 wire 版本取值。
func validateWire(wire WireVersion) []*ConfigError {
	return validateEnum("wire", wire, supportedWireVersions)
}

// validateCredentials 校验凭证集合非空，并逐条校验标识与 token。
func validateCredentials(credentials []ClientCredential) []*ConfigError {
	if len(credentials) == 0 {
		return incomplete("credentials", "必须至少配置一个客户端凭证")
	}

	problems := make([]*ConfigError, 0, len(credentials)*2)
	for index, credential := range credentials {
		field := itemField("credentials", index)
		if credential.ClientID == "" {
			problems = append(problems, newConfigError(CodeMissingAuth, field+"clientID", "必须提供客户端标识"))
		}
		if credential.Token == "" {
			problems = append(problems, newConfigError(CodeMissingAuth, field+"token", "必须提供 token"))
		}
	}
	return problems
}

// validateBindings 逐条校验代理绑定，并检测重名与对不存在客户端的引用。
func validateBindings(bindings []TCPProxyBinding, credentials []ClientCredential) []*ConfigError {
	knownClients := knownClientIDs(credentials)
	problems := make([]*ConfigError, 0, len(bindings))
	for index, binding := range bindings {
		field := itemField("bindings", index)
		problems = append(problems, validateName(field+"name", binding.Name)...)
		problems = append(problems, validatePort(field+"remotePort", binding.RemotePort)...)
		if !knownClients[binding.ClientID] {
			problems = append(problems, newConfigError(CodeUnknownClient, field+"clientID",
				"绑定的客户端标识不在已配置的凭证集合中"))
		}
	}
	return append(problems, duplicateNameProblems("bindings", bindings, func(binding TCPProxyBinding) string {
		return binding.Name
	})...)
}

// knownClientIDs 汇总凭证集合中非空的客户端标识。
func knownClientIDs(credentials []ClientCredential) map[string]bool {
	known := make(map[string]bool, len(credentials))
	for _, credential := range credentials {
		if credential.ClientID != "" {
			known[credential.ClientID] = true
		}
	}
	return known
}

// validateName 校验代理名的长度下限与上限。
func validateName(field, name string) []*ConfigError {
	if name == "" {
		return []*ConfigError{newConfigError(CodeLimitExceeded, field, "名称不能为空")}
	}
	if len(name) > MaxProxyNameLength {
		return []*ConfigError{newConfigError(CodeLimitExceeded, field,
			"名称长度超出上限 "+strconv.Itoa(MaxProxyNameLength)+" 字节")}
	}
	return nil
}

// validatePort 校验端口落在 1 到 65535 之间。
func validatePort(field string, port int) []*ConfigError {
	if port >= 1 && port <= maxPort {
		return nil
	}
	return []*ConfigError{newConfigError(CodePortOutOfRange, field,
		"端口必须在 1 到 65535 之间，不支持端口 0 自动分配")}
}

// validateTargetAddress 校验宿主指定的目标地址：必须指定 IP 与端口，且不接受未指定地址。
//
// 目标地址是整体取值，因此零值、未指定地址与端口 0 都归为地址非法；
// 独立的端口数值字段（远程端口、监听端口）则归为端口越界。
func validateTargetAddress(field string, address netip.AddrPort) []*ConfigError {
	if !address.Addr().IsValid() {
		return []*ConfigError{newConfigError(CodeInvalidAddress, field, "地址必须指定 IP 与端口")}
	}
	if address.Addr().IsUnspecified() {
		return []*ConfigError{newConfigError(CodeInvalidAddress, field, "目标地址必须指定明确主机，不接受未指定地址")}
	}
	if address.Port() == 0 {
		return []*ConfigError{newConfigError(CodeInvalidAddress, field, "目标地址端口不能为 0")}
	}
	return nil
}

// validateListenAddress 校验监听地址：允许未指定地址（表示监听全部接口），但端口必须是明确端口。
func validateListenAddress(field string, address netip.AddrPort) []*ConfigError {
	if !address.Addr().IsValid() {
		return []*ConfigError{newConfigError(CodeInvalidAddress, field, "地址必须指定 IP 与端口")}
	}
	return validatePort(field, int(address.Port()))
}

// validatePoolLimits 校验客户端侧的工作连接池上限与待命空闲上限。
func validatePoolLimits(poolSize, idleLimit int) []*ConfigError {
	problems := validateBound("workConnPoolSize", poolSize, MaxWorkConnPoolSize)
	return append(problems, validateBound("idleWorkConnLimit", idleLimit, MaxIdleWorkConnLimit)...)
}

// validateServerIdleLimit 校验服务端侧的待命工作连接空闲上限。
func validateServerIdleLimit(idleLimit int) []*ConfigError {
	return validateBound("idleWorkConnLimit", idleLimit, MaxIdleWorkConnLimit)
}

// validateBound 校验取值落在 1 到 limit 的闭区间内。
//
// 零值已在构建时被默认常量取代，因此这里的下界是 1：负值或零值都视为越界。
func validateBound(field string, value, limit int) []*ConfigError {
	if value >= 1 && value <= limit {
		return nil
	}
	return []*ConfigError{newConfigError(CodeLimitExceeded, field,
		"取值必须在 1 到 "+strconv.Itoa(limit)+" 之间")}
}

// validateEnum 校验取值是否在已交付的常量集合内，并给出受支持取值列表。
func validateEnum[T ~string](field string, value T, supported []T) []*ConfigError {
	for _, candidate := range supported {
		if value == candidate {
			return nil
		}
	}

	reason := "取值不受支持"
	if value == "" {
		reason = "取值不能为空"
	}
	names := make([]string, 0, len(supported))
	for _, candidate := range supported {
		names = append(names, string(candidate))
	}
	return []*ConfigError{newConfigError(CodeUnsupportedValue, field,
		reason+"，受支持的取值："+strings.Join(names, "、"))}
}

// validateLimit 校验条目数不超过 Core 常量上限。
func validateLimit(field string, count, limit int) []*ConfigError {
	if count <= limit {
		return nil
	}
	return []*ConfigError{newConfigError(CodeLimitExceeded, field,
		"条目数超出上限 "+strconv.Itoa(limit))}
}

// validateDurations 校验心跳间隔、超时与排水上限。
func validateDurations(heartbeat, timeout, drainTimeout time.Duration) []*ConfigError {
	problems := validateDuration("heartbeat", heartbeat)
	problems = append(problems, validateDuration("timeout", timeout)...)
	return append(problems, validateDuration("drainTimeout", drainTimeout)...)
}

// validateDuration 校验时间参数非负；零值在构建时已被默认常量取代。
func validateDuration(field string, value time.Duration) []*ConfigError {
	if value >= 0 {
		return nil
	}
	return []*ConfigError{newConfigError(CodeInvalidDuration, field, "时间参数不能为负值，零值表示使用 Core 默认值")}
}

// duplicateNameProblems 检测重名条目，字段路径指向重复出现的下标。
func duplicateNameProblems[T any](container string, items []T, nameOf func(T) string) []*ConfigError {
	seen := make(map[string]bool, len(items))
	problems := make([]*ConfigError, 0)
	for index, item := range items {
		name := nameOf(item)
		if !seen[name] {
			seen[name] = true
			continue
		}
		problems = append(problems, newConfigError(CodeDuplicateProxyName, itemField(container, index)+"name",
			"代理名重复，同一配置内的代理名必须唯一"))
	}
	return problems
}

// incomplete 构造必填字段缺失问题。
func incomplete(field, message string) []*ConfigError {
	return []*ConfigError{newConfigError(CodeIncomplete, field, message)}
}

// itemField 拼装条目字段路径前缀，例如 proxies[1].。
func itemField(container string, index int) string {
	return container + "[" + strconv.Itoa(index) + "]."
}

// collectProblems 在无问题时返回 nil，否则返回可判定的聚合错误。
func collectProblems(problems []*ConfigError) error {
	if len(problems) == 0 {
		return nil
	}
	return ConfigErrors(problems)
}
