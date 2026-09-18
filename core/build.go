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
		clientID:    draft.clientID,
		endpoint:    draft.endpoint,
		auth:        draft.auth,
		proxies:     copySlice(draft.proxies),
		heartbeat:   draft.heartbeat,
		timeout:     draft.timeout,
		hasEndpoint: draft.hasEndpoint,
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
		listen:      draft.listen,
		wire:        draft.wire,
		credentials: copySlice(draft.credentials),
		bindings:    copySlice(draft.bindings),
		heartbeat:   draft.heartbeat,
		timeout:     draft.timeout,
		hasListen:   draft.hasListen,
	}
	if err := config.Validate(); err != nil {
		return ServerConfig{}, err
	}
	return config, nil
}
