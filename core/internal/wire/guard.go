package wire

import (
	"errors"
	"io"
	"net"
	"sync"
)

// Options 是 wire 阶段的服务端本地决策。
//
// 这些取值来自服务端自身运行配置，不需要对端配合，也不写进任何协议消息。
type Options struct {
	// MaxWireVersion 是该监听入口允许的最低 wire 版本。
	// 取 VersionV2 表示仅接受 v2，对端未声明 v2 时拒绝而不是降级。
	MaxWireVersion Version
	// V2Enabled 表示服务端是否整体启用 wire v2。
	V2Enabled bool
	// Capabilities 是服务端已交付能力，供 v2 协商使用。
	Capabilities Capabilities
	// EventSink 接收结构化连接事件。为 nil 时事件被丢弃。
	EventSink EventSink
}

// CloseEvent 是一次连接关闭或拒绝的结构化事件。
//
// 事件只携带类别、阶段与脱敏后的对端地址摘要：不含对端完整载荷、密钥材料或内部字段路径。
type CloseEvent struct {
	// Stage 是发生拒绝的 wire 阶段。
	Stage Stage
	// Category 是稳定错误类别；成功关闭时为空。
	Category ErrorCategory
	// Version 是已选定的 wire 版本；未判定完成时为空。
	Version Version
	// PeerDigest 是对端地址摘要，已脱敏。
	PeerDigest string
	// Succeeded 表示该连接是否走完了 wire 阶段。
	Succeeded bool
}

// EventSink 接收连接事件。
//
// 实现必须快速返回：事件发布不得阻塞数据面。
type EventSink func(event CloseEvent)

// ConnectionGuard 持有单条连接的 wire 状态并统一处理拒绝出口。
//
// 职责：
//   - 锁定连接选定的 wire 版本，禁止同一连接内切换版本。
//   - 持有预读读取器，为 v1 解析提供回放后的流。
//   - 统一拒绝出口：记录事件、关闭连接、回收缓冲与临时状态。
type ConnectionGuard struct {
	conn    net.Conn
	pool    *BufferPool
	options Options

	mu      sync.Mutex
	version Version
	locked  bool
	closed  bool
	reader  *PeekReader
	v1      *V1Reader
	v2      *V2Reader
}

// NewConnectionGuard 建立连接守卫。
func NewConnectionGuard(conn net.Conn, pool *BufferPool, options Options) *ConnectionGuard {
	return &ConnectionGuard{conn: conn, pool: pool, options: options}
}

// Version 返回已锁定的 wire 版本；尚未判定时返回空字符串。
func (guard *ConnectionGuard) Version() Version {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.version
}

// LockVersion 锁定该连接的 wire 版本。重复锁定为不同版本时拒绝。
func (guard *ConnectionGuard) LockVersion(version Version) error {
	guard.mu.Lock()
	defer guard.mu.Unlock()

	if guard.locked && guard.version != version {
		return protocolError(CategoryVersionNotAccepted, StageDetect, "连接已选定 wire 版本，不允许切换")
	}
	guard.version = version
	guard.locked = true
	return nil
}

// requireVersion 校验连接当前是否处于给定版本，用于拒绝跨版本混入。
func (guard *ConnectionGuard) requireVersion(version Version) error {
	guard.mu.Lock()
	defer guard.mu.Unlock()

	if !guard.locked {
		return protocolError(CategoryVersionNotAccepted, StageDetect, "wire 版本尚未判定")
	}
	if guard.version != version {
		return protocolError(CategoryVersionNotAccepted, StageDetect, "该连接已选定其他 wire 版本")
	}
	return nil
}

// Closed 返回该连接是否已经走完统一拒绝出口或正常关闭。
// 供测试断言「不存在既不成功也不关闭」的中间状态。
func (guard *ConnectionGuard) Closed() bool {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.closed
}

// bindReader 保存 v1 读取器，供后续 ReadFrame 使用。
func (guard *ConnectionGuard) bindReader(reader *V1Reader) {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	guard.v1 = reader
}

// ReadFrame 按已选定的 wire 版本读取一条消息帧。
//
// v2 连接必须先完成协商：协商读取器持有流位置，不能在中途重建，否则会丢失已读字节。
func (guard *ConnectionGuard) ReadFrame() (Frame, error) {
	guard.mu.Lock()
	version := guard.version
	v1 := guard.v1
	v2 := guard.v2
	guard.mu.Unlock()

	switch version {
	case VersionV1:
		if v1 == nil {
			return Frame{}, protocolError(CategoryTransportFailure, StageMessage, "v1 读取器尚未绑定")
		}
		return guard.readV1Frame(v1)
	case VersionV2:
		if v2 == nil {
			return Frame{}, protocolError(CategoryTransportFailure, StageMessage, "v2 协商尚未完成")
		}
		return guard.readV2Frame(v2)
	default:
		return Frame{}, protocolError(CategoryVersionNotAccepted, StageMessage, "wire 版本尚未判定")
	}
}

// readV1Frame 读取一条 v1 消息帧；协议错误统一走拒绝出口。
func (guard *ConnectionGuard) readV1Frame(reader *V1Reader) (Frame, error) {
	frame, err := reader.ReadFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, err
		}
		guard.reject(err)
		return Frame{}, err
	}
	return frame, nil
}

// readV2Frame 读取一条 v2 消息帧，并把非消息帧归为帧类型非法。
func (guard *ConnectionGuard) readV2Frame(reader *V2Reader) (Frame, error) {
	frame, err := reader.ReadFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, err
		}
		guard.reject(err)
		return Frame{}, err
	}
	if frame.FrameType != V2FrameTypeMessage {
		frame.Release()
		err := protocolError(CategoryFrameTypeInvalid, StageMessage, "消息阶段收到非消息帧")
		guard.reject(err)
		return Frame{}, err
	}
	message := Frame{Type: frame.Message.Type, Payload: frame.Message.Payload, release: frame.release}
	return message, nil
}

// Negotiate 在 v2 连接上完成握手协商，并在成功后锁定协商结果。
func (guard *ConnectionGuard) Negotiate() (NegotiationResult, error) {
	if err := guard.requireVersion(VersionV2); err != nil {
		return NegotiationResult{}, guard.fail(err)
	}
	stream, ok := guard.stream()
	if !ok {
		return NegotiationResult{}, guard.fail(protocolError(CategoryTransportFailure, StageNegotiate, "协商流不可用"))
	}

	// 协商读取器必须被保留：它持有流的读取位置，后续消息帧要与 hello 共用同一读取器。
	reader := NewV2Reader(stream, DefaultV2PayloadLimit)
	reader.SetPool(guard.pool)
	helloFrame, err := reader.ReadFrame()
	if err != nil {
		return NegotiationResult{}, guard.fail(asNegotiationError(err))
	}

	request, frameErr := guard.decodeClientHello(helloFrame)
	helloFrame.Release()
	if frameErr != nil {
		return NegotiationResult{}, guard.fail(frameErr)
	}
	result, err := Negotiate(request, guard.serverCapabilities())
	if err != nil {
		return NegotiationResult{}, guard.fail(err)
	}
	// 协商上限必须回灌到消息读取路径：否则后续消息帧仍按实现上限解析，
	// 对端声明的较小上限形同虚设，违反「取双方较小值」的协商语义。
	reader.SetLimit(result.MaxPayload)

	guard.mu.Lock()
	guard.v2 = reader
	guard.mu.Unlock()
	return result, nil
}

// decodeClientHello 从协商帧中取出客户端 hello 载荷。
func (guard *ConnectionGuard) decodeClientHello(frame V2Frame) (ClientHello, error) {
	if frame.FrameType != V2FrameTypeClientHello {
		return ClientHello{}, protocolError(CategoryNegotiationFrameInvalid, StageNegotiate, "协商阶段未收到客户端 hello")
	}
	return DecodeClientHello(frame.Message.Payload)
}

// stream 返回协商可用的字节流。
func (guard *ConnectionGuard) stream() (io.Reader, bool) {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.reader != nil {
		return guard.reader, true
	}
	if guard.conn != nil {
		return guard.conn, true
	}
	return nil, false
}

// serverCapabilities 返回服务端能力，未配置时使用 P1 默认能力。
func (guard *ConnectionGuard) serverCapabilities() Capabilities {
	if len(guard.options.Capabilities.Codecs) == 0 {
		return DefaultCapabilities()
	}
	return guard.options.Capabilities
}

// asNegotiationError 把协商阶段的 hello 读取失败统一归类为协商帧非法。
//
// 判定表只区分「能力无法协商」与「协商帧非法」两类：hello 缺失、截断、超限、
// flags 非零或帧类型不符都属于后者，因此这里统一改写类别，只保留阶段信息。
func asNegotiationError(err error) error {
	if CategoryOf(err) == CategoryCapabilityMismatch {
		return err
	}
	return protocolError(CategoryNegotiationFrameInvalid, StageNegotiate, "hello 帧缺失、截断或格式非法")
}

// reject 执行统一拒绝出口：记录事件、关闭连接、回收缓冲。
func (guard *ConnectionGuard) reject(err error) {
	guard.closeWithEvent(CloseEvent{
		Stage:      stageOr(err, StageMessage),
		Category:   CategoryOf(err),
		Version:    guard.Version(),
		PeerDigest: PeerDigest(guard.conn),
		Succeeded:  false,
	})
}

// fail 返回错误并执行统一拒绝出口。
func (guard *ConnectionGuard) fail(err error) error {
	guard.reject(err)
	return err
}

// closeWithEvent 发布事件并关闭连接，保证不存在「既不成功也不关闭」的中间状态。
func (guard *ConnectionGuard) closeWithEvent(event CloseEvent) {
	guard.mu.Lock()
	if guard.closed {
		guard.mu.Unlock()
		return
	}
	guard.closed = true
	event.Version = guard.version
	guard.reader = nil
	guard.mu.Unlock()

	if guard.options.EventSink != nil {
		guard.options.EventSink(event)
	}
	if guard.conn != nil {
		_ = guard.conn.Close()
	}
}

// Close 关闭连接并发布成功关闭事件，供协商成功后的正常释放使用。
func (guard *ConnectionGuard) Close() error {
	guard.closeWithEvent(CloseEvent{
		Stage:      StageMessage,
		Version:    guard.Version(),
		PeerDigest: PeerDigest(guard.conn),
		Succeeded:  true,
	})
	return nil
}

// stageOr 返回错误携带的阶段，缺失时使用兜底阶段。
func stageOr(err error, fallback Stage) Stage {
	if stage := StageOf(err); stage != "" {
		return stage
	}
	return fallback
}
