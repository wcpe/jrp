package wire

import "sync"

// TransformState 是加密与压缩变换的状态。
//
// 控制连接与每条工作连接各自持有一份独立状态机，合法迁移路径相同：
//
//	裸连接 ──启用加密──▶ 已加密 ──启用压缩──▶ 已加密＋已压缩
//	裸连接 ──启用压缩──▶ 已压缩 ──启用加密──▶ 已加密＋已压缩
//
// 迁移判定必须在并发下保持唯一性：同一次变换只能有一个启用者成功。
type TransformState struct {
	mu          sync.Mutex
	encrypted   bool
	compressed  bool
	negotiated  NegotiationResult
	keyAssigned bool
}

// NewTransformState 在协商成功时建立状态机。
//
// 连接可用的变换集合在协商成功时就已确定：不在协商结果中的算法不允许在运行时启用。
func NewTransformState(result NegotiationResult) *TransformState {
	return &TransformState{negotiated: result}
}

// Encrypted 返回是否已启用加密。
func (state *TransformState) Encrypted() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.encrypted
}

// Compressed 返回是否已启用压缩。
func (state *TransformState) Compressed() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.compressed
}

// EnableEncryption 启用加密。
//
// 每种变换只能启用一次；重复启用、在未协商算法上启用或中途更换密钥参数都拒绝。
func (state *TransformState) EnableEncryption(key []byte) error {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.encrypted {
		return protocolError(CategoryTransformRejected, StageMessage, "加密已启用，不允许重复启用")
	}
	if state.negotiated.CryptoAlgorithm == "" || state.negotiated.CryptoAlgorithm == CryptoNone {
		return protocolError(CategoryTransformRejected, StageMessage, "协商结果未包含可用加密算法")
	}
	if len(key) == 0 {
		return protocolError(CategoryTransformRejected, StageMessage, "加密密钥材料缺失")
	}

	state.encrypted = true
	state.keyAssigned = true
	return nil
}

// EnableCompression 启用压缩。
//
// 压缩是可选变换：协商未包含压缩能力时退化为不压缩，此时启用压缩属非法迁移。
func (state *TransformState) EnableCompression() error {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.compressed {
		return protocolError(CategoryTransformRejected, StageMessage, "压缩已启用，不允许重复启用")
	}
	if state.negotiated.CompressionAlgorithm == "" || state.negotiated.CompressionAlgorithm == CompressionNone {
		return protocolError(CategoryTransformRejected, StageMessage, "协商结果未包含可用压缩算法")
	}
	state.compressed = true
	return nil
}

// RotateKey 请求更换密钥参数。
//
// 已建立的变换不可关闭、不可更换参数：连接中途要求更换一律拒绝。
func (state *TransformState) RotateKey() error {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.keyAssigned {
		return protocolError(CategoryTransformRejected, StageMessage, "已建立的变换不允许更换密钥参数")
	}
	return protocolError(CategoryTransformRejected, StageMessage, "尚未建立可更换的变换")
}

// DisableEncryption 请求关闭加密。
// 已建立的变换不可关闭，该请求永远被拒绝。
func (state *TransformState) DisableEncryption() error {
	return protocolError(CategoryTransformRejected, StageMessage, "已建立的变换不允许关闭")
}

// WriteApplicationPayload 在启用加密前拒绝写入应用载荷。
//
// 加密启用前不得向连接写入任何应用载荷：否则会出现明文泄露或两端状态不一致。
func (state *TransformState) WriteApplicationPayload(payload []byte) error {
	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.encrypted {
		return protocolError(CategoryTransformRejected, StageMessage, "加密启用前不允许写入应用载荷")
	}
	return nil
}
