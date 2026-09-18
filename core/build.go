package core

// NewClientConfig 依据选项构造客户端配置，在返回前执行完整校验。
//
// 选项只做赋值，校验集中在构建末尾一次完成，因此错误顺序确定、可复现。
// 校验失败时不返回半构造对象，而是返回聚合错误；宿主用 errors.Is(err, ErrConfigInvalid) 判定，
// 用 errors.As 取出全部 *ConfigError 一次修完。
func NewClientConfig(options ...ClientOption) (ClientConfig, error) {
	draft := &clientDraft{}
	for _, option := range options {
		option(draft)
	}
	draft.withDefaults()

	config := ClientConfig{
		clientID:     draft.clientID,
		endpoint:     draft.endpoint,
		auth:         draft.auth,
		tcpProxies:   copySlice(draft.tcpProxies),
		udpProxies:   copySlice(draft.udpProxies),
		httpProxies:  copySlice(draft.httpProxies),
		httpsProxies: copySlice(draft.httpsProxies),
		heartbeat:    draft.heartbeat,
		timeout:      draft.timeout,
		drainTimeout: draft.drainTimeout,
		hasEndpoint:  draft.hasEndpoint,
		poolSize:     draft.poolSize,
		idleLimit:    draft.idleLimit,
	}
	if err := config.Validate(); err != nil {
		return ClientConfig{}, err
	}
	return config, nil
}

// NewServerConfig 依据选项构造服务端配置，在返回前执行完整校验。
//
// 选项只做赋值，校验集中在构建末尾一次完成，因此错误顺序确定、可复现。
// 校验失败时不返回半构造对象，而是返回聚合错误；宿主用 errors.Is(err, ErrConfigInvalid) 判定，
// 用 errors.As 取出全部 *ConfigError 一次修完。
func NewServerConfig(options ...ServerOption) (ServerConfig, error) {
	draft := &serverDraft{}
	for _, option := range options {
		option(draft)
	}
	draft.withDefaults()

	config := ServerConfig{
		listen:        draft.listen,
		wire:          draft.wire,
		credentials:   copySlice(draft.credentials),
		tcpBindings:   withCopiedTargets(draft.tcpBindings),
		udpBindings:   withCopiedTargets(draft.udpBindings),
		httpBindings:  withCopiedHTTPTargets(draft.httpBindings),
		httpsBindings: withCopiedTargets(draft.httpsBindings),
		heartbeat:     draft.heartbeat,
		timeout:       draft.timeout,
		drainTimeout:  draft.drainTimeout,
		hasListen:     draft.hasListen,
		idleLimit:     draft.idleLimit,
		// UDP 参数：以下三行在 draft.withDefaults 之后读取，零值已被默认常量取代。
		udpSessionIdle:  draft.udpSessionIdle,
		udpSessionLimit: draft.udpSessionLimit,
		udpDatagramSize: draft.udpDatagramSize,
	}
	if err := config.Validate(); err != nil {
		return ServerConfig{}, err
	}
	return config, nil
}

// withCopiedTargets 深复制 TCP/UDP/HTTPS 绑定集合：逐条复制目标地址切片，
// 使宿主持有的切片在构建后无法影响配置值。
func withCopiedTargets[T interface {
	withCopiedTargets() T
}](bindings []T) []T {
	copied := make([]T, len(bindings))
	for index, binding := range bindings {
		copied[index] = binding.withCopiedTargets()
	}
	return copied
}

// withCopiedHTTPTargets 深复制 HTTP 绑定集合：目标地址与主机名切片都要复制。
func withCopiedHTTPTargets(bindings []HTTPProxyBinding) []HTTPProxyBinding {
	copied := make([]HTTPProxyBinding, len(bindings))
	for index, binding := range bindings {
		binding.AllowedTargets = copySlice(binding.AllowedTargets)
		binding.Hosts = copySlice(binding.Hosts)
		copied[index] = binding
	}
	return copied
}

// withCopiedTargets 复制单条 TCP 绑定的目标地址切片。
func (binding TCPProxyBinding) withCopiedTargets() TCPProxyBinding {
	binding.AllowedTargets = copySlice(binding.AllowedTargets)
	return binding
}

// withCopiedTargets 复制单条 UDP 绑定的目标地址切片。
func (binding UDPProxyBinding) withCopiedTargets() UDPProxyBinding {
	binding.AllowedTargets = copySlice(binding.AllowedTargets)
	return binding
}

// withCopiedTargets 复制单条 HTTPS 绑定的目标地址切片。
func (binding HTTPSProxyBinding) withCopiedTargets() HTTPSProxyBinding {
	binding.AllowedTargets = copySlice(binding.AllowedTargets)
	return binding
}
