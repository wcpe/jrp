package wire

import "encoding/json"

// 消息编码方式。
const (
	// CodecJSON 是 P1 唯一的消息编码方式。
	CodecJSON = "json"
)

// 加密算法标识。
const (
	// CryptoNone 表示不加密。
	CryptoNone = "none"
	// CryptoStreamV1 是 P1 承诺的一种对称流加密。
	CryptoStreamV1 = "jrp-stream-v1"
)

// 压缩算法标识。
const (
	// CompressionNone 表示不压缩。
	CompressionNone = "none"
)

// ClientHello 是客户端 hello 载荷。
//
// 字段表达对端能力集合，不代表任何已达成结果；协商结果由 ServerHello 承载。
// 结构按固定基线官方 frpc 的黑盒线上形状建立：能力分组在 capabilities 之下。
type ClientHello struct {
	Bootstrap    BootstrapSummary `json:"bootstrap,omitempty"`
	Capabilities ClientCaps       `json:"capabilities"`
}

// BootstrapSummary 是客户端声明的传输引导摘要。
type BootstrapSummary struct {
	Transport string `json:"transport,omitempty"`
	TLSMux    string `json:"tlsMux,omitempty"`
}

// ClientCaps 是客户端声明的能力集合。
type ClientCaps struct {
	Message     MessageCodecs     `json:"message"`
	Crypto      CryptoOffer       `json:"crypto"`
	Compression *CompressionOffer `json:"compression,omitempty"`
	MaxPayload  int               `json:"maxPayload,omitempty"`
}

// MessageCodecs 是客户端支持的消息编码集合。
type MessageCodecs struct {
	Codecs []string `json:"codecs"`
}

// CryptoOffer 是客户端支持的加密算法集合。
//
// Random 是对端生成的随机材料，只在协商期内使用；JRP 不记录、不回显。
type CryptoOffer struct {
	Algorithms []string `json:"algorithms"`
	Random     string   `json:"clientRandom,omitempty"`
}

// CompressionOffer 是客户端支持的压缩算法集合。
type CompressionOffer struct {
	Algorithms []string `json:"algorithms"`
}

// Capabilities 是服务端自身已交付的能力集合。
type Capabilities struct {
	Codecs           []string
	CryptoAlgorithms []string
	CompressionAlgos []string
	MaxPayload       int
}

// DefaultCapabilities 返回 P1 服务端默认能力。
func DefaultCapabilities() Capabilities {
	return Capabilities{
		Codecs:           []string{CodecJSON},
		CryptoAlgorithms: []string{CryptoNone, CryptoStreamV1},
		CompressionAlgos: []string{CompressionNone},
		MaxPayload:       DefaultV2PayloadLimit,
	}
}

// NegotiationResult 是协商达成后的选定结果。
//
// 结果一经产生即在该连接生命周期内锁定：不允许切换版本、编码、加密或压缩。
type NegotiationResult struct {
	MessageCodec         string `json:"messageCodec"`
	CryptoAlgorithm      string `json:"cryptoAlgorithm"`
	CompressionAlgorithm string `json:"compressionAlgorithm"`
	MaxPayload           int    `json:"maxPayload"`
}

// ServerHello 是服务端 hello 载荷。
//
// 与 client hello 对称：只承载选定结果，不回显对端能力集合。
type ServerHello struct {
	Selected SelectedCaps `json:"selected"`
}

// SelectedCaps 是协商选定的能力。
type SelectedCaps struct {
	Message     SelectedMessage      `json:"message"`
	Crypto      SelectedCrypto       `json:"crypto"`
	Compression *SelectedCompression `json:"compression,omitempty"`
	MaxPayload  int                  `json:"maxPayload"`
}

// SelectedMessage 是选定的消息编码。
type SelectedMessage struct {
	Codec string `json:"codec"`
}

// SelectedCrypto 是选定的加密算法。
//
// Random 是服务端生成的随机材料，属于协商材料而非长期密钥。
type SelectedCrypto struct {
	Algorithm string `json:"algorithm"`
	Random    string `json:"serverRandom,omitempty"`
}

// SelectedCompression 是选定的压缩算法。
type SelectedCompression struct {
	Algorithm string `json:"algorithm"`
}

// Negotiate 按服务端已交付能力计算与客户端能力的交集。
//
// 规则：
//   - 消息编码：P1 唯一可选 JSON，交集为空即协商失败。
//   - 加密：交集为空即协商失败。
//   - 压缩：可选，无交集时退化为不压缩而非失败。
//   - 单帧载荷上限：取双方较小值，且不高于 Core 自身实现上限。
func Negotiate(request ClientHello, server Capabilities) (NegotiationResult, error) {
	codec, ok := intersectPreferServer(request.Capabilities.Message.Codecs, server.Codecs)
	if !ok {
		return NegotiationResult{}, protocolError(CategoryCapabilityMismatch, StageNegotiate, "消息编码无共同支持项")
	}
	crypto, ok := intersectPreferServer(request.Capabilities.Crypto.Algorithms, server.CryptoAlgorithms)
	if !ok {
		return NegotiationResult{}, protocolError(CategoryCapabilityMismatch, StageNegotiate, "加密算法无共同支持项")
	}

	compression := CompressionNone
	if offer := request.Capabilities.Compression; offer != nil {
		if selected, ok := intersectPreferServer(offer.Algorithms, server.CompressionAlgos); ok {
			compression = selected
		}
	}

	return NegotiationResult{
		MessageCodec:         codec,
		CryptoAlgorithm:      crypto,
		CompressionAlgorithm: compression,
		MaxPayload:           negotiatePayloadLimit(request.Capabilities.MaxPayload, server.MaxPayload),
	}, nil
}

// intersectPreferServer 在双方集合中求交集，返回服务端侧优先顺序的首个共同项。
//
// 取服务端顺序而非客户端顺序，避免对端通过排列顺序影响服务端选择。
func intersectPreferServer(clientValues, serverValues []string) (string, bool) {
	if len(clientValues) == 0 || len(serverValues) == 0 {
		return "", false
	}
	clientSet := make(map[string]struct{}, len(clientValues))
	for _, value := range clientValues {
		clientSet[value] = struct{}{}
	}
	for _, value := range serverValues {
		if _, ok := clientSet[value]; ok {
			return value, true
		}
	}
	return "", false
}

// negotiatePayloadLimit 取双方声明的较小值，并受 Core 实现上限约束。
//
// 对端未声明上限时按服务端上限处理；声明值为不可能取值时按服务端上限处理，
// 因为上限只会收紧不会放宽，不做静默放大。
func negotiatePayloadLimit(clientLimit, serverLimit int) int {
	limit := serverLimit
	if clientLimit > 0 && clientLimit < limit {
		limit = clientLimit
	}
	if limit > DefaultV2PayloadLimit {
		limit = DefaultV2PayloadLimit
	}
	return limit
}

// EncodeClientHello 编码客户端 hello 载荷。
func EncodeClientHello(hello ClientHello) ([]byte, error) {
	return encodeHelloPayload(hello, StageNegotiate)
}

// DecodeClientHello 解码客户端 hello 载荷。
//
// 载荷结构畸形属于「协商帧非法」，与能力交集为空是不同类别。
func DecodeClientHello(payload []byte) (ClientHello, error) {
	var hello ClientHello
	if err := decodeHelloPayload(payload, &hello); err != nil {
		return ClientHello{}, err
	}
	return hello, nil
}

// EncodeServerHello 编码服务端 hello 载荷（不含帧头）。
func EncodeServerHello(result NegotiationResult) ([]byte, error) {
	hello := ServerHello{
		Selected: SelectedCaps{
			Message:    SelectedMessage{Codec: result.MessageCodec},
			Crypto:     SelectedCrypto{Algorithm: result.CryptoAlgorithm},
			MaxPayload: result.MaxPayload,
		},
	}
	if result.CompressionAlgorithm != "" && result.CompressionAlgorithm != CompressionNone {
		hello.Selected.Compression = &SelectedCompression{Algorithm: result.CompressionAlgorithm}
	}
	return encodeHelloPayload(hello, StageNegotiate)
}

// DecodeServerHello 解码服务端 hello 载荷。
func DecodeServerHello(payload []byte) (NegotiationResult, error) {
	var hello ServerHello
	if err := decodeHelloPayload(payload, &hello); err != nil {
		return NegotiationResult{}, err
	}
	compression := CompressionNone
	if hello.Selected.Compression != nil {
		compression = hello.Selected.Compression.Algorithm
	}
	return NegotiationResult{
		MessageCodec:         hello.Selected.Message.Codec,
		CryptoAlgorithm:      hello.Selected.Crypto.Algorithm,
		CompressionAlgorithm: compression,
		MaxPayload:           hello.Selected.MaxPayload,
	}, nil
}

// decodeHelloPayload 解码 hello 载荷，畸形结构归类为协商帧非法。
func decodeHelloPayload(payload []byte, target any) error {
	if err := json.Unmarshal(payload, target); err != nil {
		return protocolError(CategoryNegotiationFrameInvalid, StageNegotiate, "hello 载荷结构畸形")
	}
	return nil
}

// encodeHelloPayload 编码 hello 载荷。
func encodeHelloPayload(value any, stage Stage) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, protocolError(CategoryNegotiationFrameInvalid, stage, "hello 载荷无法编码")
	}
	return encoded, nil
}

// EncodeNegotiationFailure 生成安全可公开的失败原因。
//
// 失败原因只含类别信息：不回显密钥材料、内部字段路径或完整对端载荷。
func EncodeNegotiationFailure(category ErrorCategory) string {
	return string(category)
}
