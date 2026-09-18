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

// DefaultDrainTimeout 是排水上限为零值时采用的 Core 默认值。
//
// 排水上限是 Shutdown 等待活动连接自然结束的最长时间；超过上限强制释放。
const DefaultDrainTimeout = 10 * time.Second

// DefaultWorkConnPoolSize 是工作连接池上限为零值时采用的 Core 默认值。
//
// 取值 1 表示每个代理同时只有一条工作连接在途：这是当前转发语义下的最小
// 正确值，放大取值需要上层具备连接复用能力。
const DefaultWorkConnPoolSize = 1

// MaxWorkConnPoolSize 是单代理工作连接池上限的上界。
//
// 池容量直接决定每代理的在途连接与转发 goroutine 数量，必须由 Core 常量约束
// 上界，避免宿主配置把单机资源打满。
const MaxWorkConnPoolSize = 16

// DefaultIdleWorkConnLimit 是待命工作连接空闲上限为零值时采用的 Core 默认值。
const DefaultIdleWorkConnLimit = 2

// MaxIdleWorkConnLimit 是单代理待命工作连接空闲上限的上界。
//
// 待命连接已建链但不承载数据，每条都占用两端的文件描述符与暂存内存，必须由
// Core 常量约束上界。
const MaxIdleWorkConnLimit = 16
