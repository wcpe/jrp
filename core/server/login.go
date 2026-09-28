package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"
)

// 稳定失败类别（FR-03 规格 §3.4/§5）：登录失败只回显类别，不回显 token、
// 摘要、时间材料原文或内部校验细节。类别文本是契约的一部分，保持稳定。
var (
	ErrLoginTokenMismatch  = errors.New("login_failed: token")
	ErrLoginTimeWindow     = errors.New("login_failed: time_window")
	ErrLoginReplay         = errors.New("login_failed: replay")
	ErrLoginWireCapability = errors.New("login_failed: wire_capability")
)

// 默认登录链参数。
const (
	// DefaultLoginTimeWindow 是鉴权时间材料的接受窗口（±）。
	// 官方 frpc 与 JRP 客户端之间的时钟偏差在分钟级即可接受；取值过大
	// 会拉长重放窗口，过小则误伤慢网络下的正常登录。
	DefaultLoginTimeWindow = 2 * time.Minute
	// DefaultReplayCacheSize 是单个会话重放缓存的最大条目数。
	// 登录频率低（每个客户端一次会话），小缓存配合最旧淘汰即可。
	DefaultReplayCacheSize = 64
)

// loginChainConfig 是登录校验链的参数。
type loginChainConfig struct {
	// TimeWindow 是时间材料接受窗口（±），零值取 DefaultLoginTimeWindow。
	TimeWindow time.Duration
	// ReplayCacheSize 是重放缓存上限，零值取 DefaultReplayCacheSize。
	ReplayCacheSize int
}

// loginChain 是单个会话的登录校验链。
//
// 它承载时间窗口与重放边界两个有状态校验；token 摘要比较无状态，由
// credentialsMatch 直接完成。链本身只被该会话的读循环使用，与数据面无关。
type loginChain struct {
	window  time.Duration
	recent  map[runKey]struct{}
	order   []runKey
	maxSeen int
}

// runKey 标识一次登录尝试：运行 ID 与时间戳的组合。
type runKey struct {
	runID     string
	timestamp int64
}

// newLoginChain 构造校验链；参数为零值时取默认值。
func newLoginChain(config loginChainConfig) *loginChain {
	window := config.TimeWindow
	if window <= 0 {
		window = DefaultLoginTimeWindow
	}
	size := config.ReplayCacheSize
	if size <= 0 {
		size = DefaultReplayCacheSize
	}
	return &loginChain{
		window:  window,
		recent:  make(map[runKey]struct{}, size),
		maxSeen: size,
	}
}

// checkTimeWindow 校验时间材料是否在接受窗口内。
//
// 材料是 Unix 毫秒时间戳；超前或滞后超出窗口都拒绝——滞后过多是重放的主要
// 形态，超前过多则说明对端时钟不可信。
func (chain *loginChain) checkTimeWindow(timestampMillis int64) error {
	skew := time.Since(time.UnixMilli(timestampMillis))
	if skew < 0 {
		skew = -skew
	}
	if skew > chain.window {
		return ErrLoginTimeWindow
	}
	return nil
}

// checkReplay 校验并登记一次登录尝试。
//
// 同一 runKey 的第二次提交判为重放；缓存超过上限时淘汰最旧条目。
// 返回 ErrLoginReplay 的语义是「这份材料已被使用」，调用方据此关闭连接。
func (chain *loginChain) checkReplay(runID string, timestampMillis int64) error {
	key := runKey{runID: runID, timestamp: timestampMillis}
	if _, seen := chain.recent[key]; seen {
		return ErrLoginReplay
	}
	if len(chain.recent) >= chain.maxSeen {
		// 淘汰最旧条目：order 与插入顺序一致。
		delete(chain.recent, chain.order[0])
		chain.order = chain.order[1:]
	}
	chain.recent[key] = struct{}{}
	chain.order = append(chain.order, key)
	return nil
}

// DigestToken 计算登录材料的 SHA-256 摘要。
//
// 快照凭证集合持有摘要而不是明文：数据面与 FR-07 的摘要真源统一（官方 frpc
// 的明文 token 只存在于其自身配置文件中，到达服务端后立即转为摘要比对）。
// 宿主装配凭证快照时也使用本函数（例如 jrps 从客户端表取摘要后直接填充）。
func DigestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// digestEqual 以恒定时间比较两个摘要，避免比较耗时差异泄漏匹配进度。
func digestEqual(providedDigest, storedDigest string) bool {
	return subtle.ConstantTimeCompare([]byte(providedDigest), []byte(storedDigest)) == 1
}
