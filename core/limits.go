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
// 取值与 UDP 会话上限（DefaultUDPSessionLimit，8）对齐：UDP 代理按对端地址
// 会话化，每条会话独占一条工作连接并在整个会话生命周期内持有，因此池上限小于
// 会话上限时，第 2 个对端起就取不到工作连接——表现为"多用户共享一个 UDP 代理
// 时随机只有一个可用"。取 1（原先的取值）正是这种情形。
//
// 对 TCP 与 HTTP 代理，放大该值不改变单访客的连接行为，只允许更多访客同时
// 建立桥接：客户端按代理名循环补充待命连接，每条连接用一次即释放槽位。
//
// 池只记账容量、不预分配连接，因此放大取值不增加空闲资源占用。
// 上界仍由 MaxWorkConnPoolSize 约束。
const DefaultWorkConnPoolSize = 8

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

// DefaultUDPSessionIdle 是 UDP 会话空闲上限为零值时采用的 Core 默认值。
//
// 规格 §6 把该取值列为待定项：这里按最小可用选取，使其在常见 NAT 映射超时
// 之前回收而不过早切断突发间隔较长的会话，真实网络下的合适取值需用户实机确认。
const DefaultUDPSessionIdle = 60 * time.Second

// DefaultUDPSessionLimit 是单个 UDP 代理的会话数上限为零值时采用的 Core 默认值。
const DefaultUDPSessionLimit = 8

// MaxUDPSessionLimit 是单代理 UDP 会话数上限的上界。
//
// 每条会话独占一条工作连接与两个 goroutine，必须由 Core 常量约束上界。
const MaxUDPSessionLimit = 256

// DefaultUDPDatagramSize 是单个 UDP 数据报字节上限为零值时采用的 Core 默认值。
//
// 取值等于以太网 MTU 减去 IP 与 UDP 头部后的常见安全载荷，避免分片；
// 需要更大的数据报时宿主应显式配置。
const DefaultUDPDatagramSize = 1400

// MaxUDPDatagramSize 是单个 UDP 数据报字节上限的上界，等于 IPv4 下 UDP 报文的最大载荷。
const MaxUDPDatagramSize = 65507

// MaxHTTPRouteCount 是单个入口端口上 HTTP 路由条数的上限。
//
// 每条路由参与一次线性匹配，必须由 Core 常量约束上界，避免线性查找无界增长。
const MaxHTTPRouteCount = 256

// MaxAllowTargetCount 是单个代理允许的目标地址条目数上限。
//
// 目标地址集合逐条参与越权判定，必须由 Core 常量约束上界。
const MaxAllowTargetCount = 16
