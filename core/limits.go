package core

import "time"

// MaxProxyCount 是单份配置允许的代理条目数上限。
//
// 取值与 NFR 规模基线一致（单机 200 个在线客户端、2,000 个代理），
// 由 Core 常量定义、不提供宿主可调参数，避免无界分配。
const MaxProxyCount = 2000

// MaxClientCredentialCount 是服务端配置允许的客户端凭证数上限，对应 NFR 规模基线中的 200 个在线客户端。
const MaxClientCredentialCount = 200

// MaxProxyNameLength 是代理名的字节长度上限。
//
// 上限保证名称可用于路由键与日志而不引入无界分配；空名与超长名都按超出上限处理。
const MaxProxyNameLength = 64

// DefaultHeartbeat 是心跳间隔为零值时采用的 Core 默认值。
const DefaultHeartbeat = 30 * time.Second

// DefaultTimeout 是超时为零值时采用的 Core 默认值。
const DefaultTimeout = 10 * time.Second
