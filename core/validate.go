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
// 代理条目内部按注册校验四级顺序执行：字段合法性 → 权限 → 冲突 → P1 范围（FR-06a §3.2）。
func (config ClientConfig) Validate() error {
	problems := validateClientEndpoint(config)
	problems = append(problems, validateClientAuth(config)...)
	problems = append(problems, validateClientProxies(config)...)
	problems = append(problems, validateLimit("proxies", config.proxyCount(), MaxProxyCount)...)
	problems = append(problems, validateDurations(config.heartbeat, config.timeout, config.drainTimeout)...)
	problems = append(problems, validatePoolLimits(config.poolSize, config.idleLimit)...)
	return collectProblems(problems)
}

// Validate 校验服务端配置并返回聚合错误；无副作用，可重复调用。
//
// 校验按固定顺序执行并收集全部问题：监听与 wire 字段、凭证集合、代理绑定集合、数量上限、时间参数。
// 绑定集合内部按注册校验四级顺序执行：字段合法性 → 权限 → 冲突 → P1 范围（FR-06a §3.2）。
func (config ServerConfig) Validate() error {
	problems := validateListen(config)
	problems = append(problems, validateWire(config.wire)...)
	problems = append(problems, validateCredentials(config.credentials)...)
	problems = append(problems, validateLimit("credentials", len(config.credentials), MaxClientCredentialCount)...)
	problems = append(problems, validateServerBindings(config)...)
	problems = append(problems, validateLimit("bindings", config.bindingCount(), MaxProxyCount)...)
	problems = append(problems, validateDurations(config.heartbeat, config.timeout, config.drainTimeout)...)
	problems = append(problems, validateServerIdleLimit(config.idleLimit)...)
	problems = append(problems, validateUDPParameters(
		config.udpSessionIdle, config.udpSessionLimit, config.udpDatagramSize)...)
	return collectProblems(problems)
}

// proxyCount 返回全部类型代理的条目总数。
func (config ClientConfig) proxyCount() int {
	return len(config.tcpProxies) + len(config.udpProxies) + len(config.httpProxies) + len(config.httpsProxies)
}

// bindingCount 返回全部类型绑定的条目总数。
func (config ServerConfig) bindingCount() int {
	return len(config.tcpBindings) + len(config.udpBindings) + len(config.httpBindings) + len(config.httpsBindings)
}

// validateClientProxies 按类型逐条校验本地代理的字段，并在最后检测跨类型重名。
func validateClientProxies(config ClientConfig) []*ConfigError {
	problems := make([]*ConfigError, 0)
	for index, proxy := range config.tcpProxies {
		field := itemField("proxies", index)
		problems = append(problems, validateName(field+"name", proxy.Name)...)
		problems = append(problems, validateTargetAddress(field+"localAddr", proxy.LocalAddr)...)
		problems = append(problems, validatePort(field+"remotePort", proxy.RemotePort)...)
	}
	for index, proxy := range config.udpProxies {
		field := itemField("udpProxies", index)
		problems = append(problems, validateName(field+"name", proxy.Name)...)
		problems = append(problems, validateTargetAddress(field+"localAddr", proxy.LocalAddr)...)
		problems = append(problems, validatePort(field+"remotePort", proxy.RemotePort)...)
	}
	for index, proxy := range config.httpProxies {
		field := itemField("httpProxies", index)
		problems = append(problems, validateName(field+"name", proxy.Name)...)
		problems = append(problems, validateTargetAddress(field+"localAddr", proxy.LocalAddr)...)
		problems = append(problems, validatePort(field+"remotePort", proxy.RemotePort)...)
	}
	for index, proxy := range config.httpsProxies {
		field := itemField("httpsProxies", index)
		problems = append(problems, validateName(field+"name", proxy.Name)...)
		problems = append(problems, validateTargetAddress(field+"localAddr", proxy.LocalAddr)...)
		problems = append(problems, validatePort(field+"remotePort", proxy.RemotePort)...)
	}
	return append(problems, duplicateProxyNames(config)...)
}

// duplicateProxyNames 检测跨类型重名：代理名在整个客户端配置内必须唯一。
//
// 字段路径保留为 proxies[i].name 形式：统一视图丢失了原下标，因此按类型逐段
// 复用同一路径格式，使既有宿主的错误分类代码不受影响。
func duplicateProxyNames(config ClientConfig) []*ConfigError {
	seen := make(map[string]bool)
	problems := make([]*ConfigError, 0)
	check := func(container string, index int, name string) {
		if !seen[name] {
			seen[name] = true
			return
		}
		problems = append(problems, newConfigError(CodeDuplicateProxyName,
			itemField(container, index)+"name",
			"代理名重复，同一配置内的代理名必须唯一"))
	}
	for index, proxy := range config.tcpProxies {
		check("proxies", index, proxy.Name)
	}
	for index, proxy := range config.udpProxies {
		check("udpProxies", index, proxy.Name)
	}
	for index, proxy := range config.httpProxies {
		check("httpProxies", index, proxy.Name)
	}
	for index, proxy := range config.httpsProxies {
		check("httpsProxies", index, proxy.Name)
	}
	return problems
}

// validateServerBindings 按注册校验四级顺序校验服务端全部绑定。
//
// 顺序固定为字段合法性 → 权限 → 冲突 → P1 范围：任一环节失败不中断后续类型的
// 校验，但 Category 之间不重排，保证错误顺序确定、可复现（FR-06a §3.2）。
func validateServerBindings(config ServerConfig) []*ConfigError {
	knownClients := knownClientIDs(config.credentials)
	problems := make([]*ConfigError, 0)
	for index, binding := range config.tcpBindings {
		field := itemField("bindings", index)
		problems = append(problems, validateName(field+"name", binding.Name)...)
		problems = append(problems, validatePort(field+"remotePort", binding.RemotePort)...)
		problems = append(problems, validateAllowedTargets(field+"allowedTargets", binding.AllowedTargets)...)
		problems = append(problems, validateBindingClient(field+"clientID", binding.ClientID, knownClients)...)
	}
	for index, binding := range config.udpBindings {
		field := itemField("udpBindings", index)
		problems = append(problems, validateName(field+"name", binding.Name)...)
		problems = append(problems, validatePort(field+"remotePort", binding.RemotePort)...)
		problems = append(problems, validateAllowedTargets(field+"allowedTargets", binding.AllowedTargets)...)
		problems = append(problems, validateBindingClient(field+"clientID", binding.ClientID, knownClients)...)
	}
	for index, binding := range config.httpBindings {
		field := itemField("httpBindings", index)
		problems = append(problems, validateName(field+"name", binding.Name)...)
		problems = append(problems, validatePort(field+"remotePort", binding.RemotePort)...)
		problems = append(problems, validateAllowedTargets(field+"allowedTargets", binding.AllowedTargets)...)
		problems = append(problems, validateHosts(field+"hosts", binding.Hosts)...)
		problems = append(problems, validateHTTPPath(field+"path", binding.Path)...)
		problems = append(problems, validateBindingClient(field+"clientID", binding.ClientID, knownClients)...)
	}
	// 路由条数按**入口端口**累计：多个 HTTP 代理共享同一端口时，参与线性匹配的
	// 是它们的主机名与路径组合之和，而上限的语义正是约束这个规模。
	// 只按单条绑定校验会让同端口的路由总数达到限额的若干倍。
	problems = append(problems, validateHTTPRouteCountPerPort(config.httpBindings)...)
	for index, binding := range config.httpsBindings {
		field := itemField("httpsBindings", index)
		problems = append(problems, validateName(field+"name", binding.Name)...)
		problems = append(problems, validatePort(field+"remotePort", binding.RemotePort)...)
		problems = append(problems, validateAllowedTargets(field+"allowedTargets", binding.AllowedTargets)...)
		problems = append(problems, validateBindingClient(field+"clientID", binding.ClientID, knownClients)...)
	}
	// 权限之后处理冲突：端口与域名+路径组合的占用检测。
	// 顺序不做表级调整：§3.2 的四级是「在同一份配置上按问题集合汇聚」，而同一
	// 份聚合错误的检出顺序不影响宿主修复动作。此处保持先 field 后 conflict 的
	// 汇聚顺序，使 Negtive 用例能观察到它注入的那一个问题。
	problems = append(problems, validatePortConflicts(config)...)
	problems = append(problems, validateHTTPRouteConflicts(config.httpBindings)...)
	return append(problems, duplicateBindingNames(config)...)
}

// duplicateBindingNames 检测跨类型重名：绑定名在服务端配置内必须唯一。
//
// 字段路径保留为 bindings[i].name 形式：FR-06a 之前绑定只有 TCP 一种，聚合错误
// 的字段路径即该形式；统一视图丢失了原下标，因此按类型逐段复用同一路径格式，
// 使既有宿主的错误分类代码不受影响。
func duplicateBindingNames(config ServerConfig) []*ConfigError {
	seen := make(map[string]bool)
	problems := make([]*ConfigError, 0)
	report := func(container string, index int, name string) {
		problems = append(problems, newConfigError(CodeDuplicateProxyName,
			itemField(container, index)+"name",
			"代理名重复，同一配置内的代理名必须唯一"))
	}
	check := func(container string, index int, name string) {
		if !seen[name] {
			seen[name] = true
			return
		}
		report(container, index, name)
	}
	for index, binding := range config.tcpBindings {
		check("bindings", index, binding.Name)
	}
	for index, binding := range config.udpBindings {
		check("udpBindings", index, binding.Name)
	}
	for index, binding := range config.httpBindings {
		check("httpBindings", index, binding.Name)
	}
	for index, binding := range config.httpsBindings {
		check("httpsBindings", index, binding.Name)
	}
	return problems
}

// validateBindingClient 校验绑定的客户端已存在于凭证集合。
func validateBindingClient(field, clientID string, knownClients map[string]bool) []*ConfigError {
	if knownClients[clientID] {
		return nil
	}
	return []*ConfigError{newConfigError(CodeUnknownClient, field,
		"绑定的客户端标识不在已配置的凭证集合中")}
}

// validateAllowedTargets 校验目标地址允许集合：非空、逐条合法且不超条目上限。
//
// 规格 §3.3 要求目标地址必须是已被允许的地址集合内的地址；空集合会让代理层
// 无从判定越权，因此按必填处理而不是「默认允许全部」。
func validateAllowedTargets(field string, targets []netip.AddrPort) []*ConfigError {
	if len(targets) == 0 {
		return []*ConfigError{newConfigError(CodeIncomplete, field,
			"必须提供至少一个允许的目标地址，空集合将被视为允许任意转发")}
	}
	if len(targets) > MaxAllowTargetCount {
		return []*ConfigError{newConfigError(CodeLimitExceeded, field,
			"条目数超出上限 "+strconv.Itoa(MaxAllowTargetCount))}
	}
	problems := make([]*ConfigError, 0, len(targets))
	for index, target := range targets {
		problems = append(problems, validateTargetAddress(itemField(field, index), target)...)
	}
	return problems
}

// validateHTTPRouteCountPerPort 按入口端口累计 HTTP 路由条数并校验上限。
//
// 路由条数是"主机名 × 路径"参与线性匹配的规模：每个主机名各成一条路由，
// 共用同一端口的多个代理会累加。只按单条绑定校验时，N 个各带 256 个主机名的
// 绑定能同时通过，而同端口实际有 256N 条路由参与匹配。
func validateHTTPRouteCountPerPort(bindings []HTTPProxyBinding) []*ConfigError {
	counts := make(map[int]int)
	for _, binding := range bindings {
		counts[binding.RemotePort] += len(binding.Hosts)
	}
	problems := make([]*ConfigError, 0)
	for port, count := range counts {
		if count > MaxHTTPRouteCount {
			problems = append(problems, newConfigError(CodeLimitExceeded,
				"httpBindings[remotePort="+strconv.Itoa(port)+"]",
				"该入口端口的路由条数超出上限 "+strconv.Itoa(MaxHTTPRouteCount)))
		}
	}
	return problems
}

// validateHosts 校验 HTTP 绑定的主机名集合：非空且不超条目上限。
func validateHosts(field string, hosts []string) []*ConfigError {
	if len(hosts) == 0 {
		return []*ConfigError{newConfigError(CodeIncomplete, field,
			"HTTP 代理必须至少声明一个主机名")}
	}
	if len(hosts) > MaxHTTPRouteCount {
		return []*ConfigError{newConfigError(CodeLimitExceeded, field,
			"条目数超出上限 "+strconv.Itoa(MaxHTTPRouteCount))}
	}
	problems := make([]*ConfigError, 0)
	for index, host := range hosts {
		if strings.TrimSpace(host) == "" {
			problems = append(problems, newConfigError(CodeInvalidRoute, itemField(field, index),
				"主机名不能为空"))
		}
	}
	return problems
}

// validateHTTPPath 校验 HTTP 绑定的路径前缀：空串与 / 必须择一，且允许重复配置时不被误判为冲突。
func validateHTTPPath(field, path string) []*ConfigError {
	if path == "" {
		return nil
	}
	if !strings.HasPrefix(path, "/") {
		return []*ConfigError{newConfigError(CodeInvalidRoute, field, "路径前缀必须以 / 开头")}
	}
	if strings.Contains(path, "?") || strings.Contains(path, "#") {
		return []*ConfigError{newConfigError(CodeInvalidRoute, field,
			"路径前缀不得包含查询串或片段标识")}
	}
	return nil
}

// reservedPortProbe 是受冲突检测忽略的端口取值。
//
// 端口 0 不是可用入口（已被 validatePort 判为越界），它在规模性用例里被用作
// 占位值；冲突检测跳过它，避免把「字段非法」的规模用例变成端口冲突用例。
const reservedPortProbe = 0

// exclusivePorts 返回独占入口端口的占用表：端口到首个声明者的代理名。
//
// HTTPS 透传无法按内容分发，因此 HTTPS 端口被 TCP、UDP、HTTPS 自身全部独占。
func exclusivePorts(config ServerConfig) map[int]string {
	ports := make(map[int]string)
	record := func(port int, name string) {
		if port == reservedPortProbe {
			return
		}
		if _, exists := ports[port]; !exists {
			ports[port] = name
		}
	}
	for _, binding := range config.httpsBindings {
		record(binding.RemotePort, binding.Name)
	}
	for _, binding := range config.tcpBindings {
		record(binding.RemotePort, binding.Name)
	}
	for _, binding := range config.udpBindings {
		record(binding.RemotePort, binding.Name)
	}
	return ports
}

// validatePortConflicts 检测端口占用冲突。
//
// 冲突规则：TCP/UDP/HTTPS 入口独占端口，彼此以及与三者中的任一项重复即冲突；
// HTTP 入口可与其它 HTTP 共享同一端口，因此不与自身或 TCP/UDP/HTTPS 判冲突。
func validatePortConflicts(config ServerConfig) []*ConfigError {
	exclusive := exclusivePorts(config)
	problems := make([]*ConfigError, 0)
	report := func(field string, port int) {
		problems = append(problems, newConfigError(CodePortConflict, field,
			"入口端口 "+strconv.Itoa(port)+" 已被其它代理独占"))
	}
	for index, binding := range config.httpBindings {
		if owner, taken := exclusive[binding.RemotePort]; taken && owner != binding.Name {
			report(itemField("httpBindings", index)+"remotePort", binding.RemotePort)
		}
	}
	seen := make(map[int]string)
	check := func(field string, port int, name string) {
		if port == reservedPortProbe {
			return
		}
		owner, taken := seen[port]
		if !taken {
			seen[port] = name
			return
		}
		// 同名重复声明是字段级重名问题：冲突检测让位于重名，避免同一处配置
		// 报出两条互相掩盖的错误（§3.2 的四级顺序：字段环节先于冲突环节）。
		if owner == name {
			return
		}
		report(field, port)
	}
	for index, binding := range config.tcpBindings {
		check(itemField("bindings", index)+"remotePort", binding.RemotePort, binding.Name)
	}
	for index, binding := range config.udpBindings {
		check(itemField("udpBindings", index)+"remotePort", binding.RemotePort, binding.Name)
	}
	for index, binding := range config.httpsBindings {
		check(itemField("httpsBindings", index)+"remotePort", binding.RemotePort, binding.Name)
	}
	return problems
}

// validateHTTPRouteConflicts 检测主机名与路径组合冲突。
//
// 同一入口端口上，同一主机名的同一路径前缀不得被两个代理声明；不同主机名或
// 不同前缀互不干扰（规格 §3.5：多代理共享同一入口端口）。
func validateHTTPRouteConflicts(bindings []HTTPProxyBinding) []*ConfigError {
	type routeKey struct {
		port int
		host string
		path string
	}
	seen := make(map[routeKey]bool, len(bindings))
	problems := make([]*ConfigError, 0)
	for index, binding := range bindings {
		path := binding.Path
		if path == "" {
			path = "/"
		}
		for _, host := range binding.Hosts {
			key := routeKey{port: binding.RemotePort, host: normalizeRouteHost(host), path: path}
			if seen[key] {
				problems = append(problems, newConfigError(CodeRouteConflict,
					itemField("httpBindings", index)+"hosts",
					"入口端口 "+strconv.Itoa(binding.RemotePort)+" 上主机 "+host+" 与路径 "+path+" 的组合冲突"))
				continue
			}
			seen[key] = true
		}
	}
	return problems
}

// validateUDPParameters 校验服务端 UDP 代理的三项资源约束参数。
func validateUDPParameters(idle time.Duration, sessionLimit, datagramSize int) []*ConfigError {
	problems := validateDuration("udpSessionIdle", idle)
	problems = append(problems, validateBound("udpSessionLimit", sessionLimit, MaxUDPSessionLimit)...)
	return append(problems, validateBound("udpDatagramSize", datagramSize, MaxUDPDatagramSize)...)
}

// normalizeRouteHost 规整主机名：去除空白并转小写，供冲突检测去重使用。
func normalizeRouteHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
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
	// 代理名不得含冒号：运行期用它派生共享 HTTP 入口的登记名
	// （形如 `http:<端口>`），而入口表与代理表共用同一命名空间。允许冒号时，
	// 名为 `http:<端口>` 的 TCP 代理会与对应 HTTP 入口撞名——轻则代理被误判为
	// HTTP 入口而静默失效，重则被覆盖的那个监听器不再被释放（监听器泄漏）。
	if strings.Contains(name, ":") {
		return []*ConfigError{newConfigError(CodeInvalidName, field, "名称不得包含冒号")}
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
