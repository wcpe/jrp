package wire

import (
	"bytes"
	"errors"
	"io"
)

// ErrEmptyConnection 表示连接在写出任何字节之前就已结束。
// 空连接既不算协议错误也不算成功，调用方应直接关闭。
var ErrEmptyConnection = errors.New("空连接：未收到任何字节即结束")

// PeekReader 是带回放能力的预读读取器。
//
// wire 版本判定必须读取固定宽度的前导字节，而 v1 没有独立魔数：一旦判定失败却
// 消费掉前导字节，v1 解析将永久错位。因此预读字节必须完整回放，读取顺序对外
// 表现为「从未读过的原始流」。
type PeekReader struct {
	source io.Reader
	peeked []byte
	offset int
}

// NewPeekReader 建立预读读取器。
func NewPeekReader(source io.Reader) *PeekReader {
	return &PeekReader{source: source}
}

// Peek 预读最多 size 字节且不消费。
//
// 返回的切片是内部缓冲视图，调用方只读不写。
// 连接在预读期间结束且已读字节不足 size 时，返回已读字节与 io.EOF 之外的错误。
func (reader *PeekReader) Peek(size int) ([]byte, error) {
	if len(reader.peeked) < size {
		reader.compact()
		missing := size - len(reader.peeked)
		buffer := make([]byte, missing)
		read, err := io.ReadFull(reader.source, buffer)
		reader.peeked = append(reader.peeked, buffer[:read]...)
		if err != nil {
			return reader.peeked, err
		}
	}
	return reader.peeked[:size], nil
}

// Read 实现 io.Reader：先回放已预读字节，再继续读取底层流。
func (reader *PeekReader) Read(p []byte) (int, error) {
	if reader.offset < len(reader.peeked) {
		read := copy(p, reader.peeked[reader.offset:])
		reader.offset += read
		if read == len(p) {
			return read, nil
		}
		p = p[read:]
		if reader.offset >= len(reader.peeked) {
			reader.compact()
			if len(p) == 0 {
				return read, nil
			}
		}
		remaining, err := reader.source.Read(p)
		return read + remaining, err
	}
	return reader.source.Read(p)
}

// Replay 把全部已预读字节回放到流前端。
//
// 预读字节仍在内部缓冲中，回放只是把读游标复位，因此不会丢失任何字节，
// 也不需要额外的拷贝或 pushback 缓冲。
func (reader *PeekReader) Replay() {
	reader.offset = 0
}

// Consume 消费指定数量的已预读字节。
//
// v2 魔数是连接级前缀而非任何帧的一部分：判定为 v2 后必须消费它，
// 否则协商读取器会把魔数当成帧头解析。
func (reader *PeekReader) Consume(size int) {
	if size <= 0 {
		return
	}
	if size > len(reader.peeked) {
		size = len(reader.peeked)
	}
	reader.offset = size
}

// compact 在预读缓冲全部消费后释放引用，避免长期持有已回放数据。
func (reader *PeekReader) compact() {
	if reader.offset >= len(reader.peeked) {
		reader.peeked = nil
		reader.offset = 0
	}
}

// Version 是已选定的 wire 版本。
type Version string

const (
	// VersionV1 是 wire v1。
	VersionV1 Version = "v1"
	// VersionV2 是 wire v2。
	VersionV2 Version = "v2"
)

// DetectVersion 通过预读魔数判定 wire 版本。
//
// 判定规则：
//  1. 前导字节等于 v2 魔数 → v2。
//  2. 前导字节不足且连接已结束 → 空连接。
//  3. 前导字节不等于魔数 → v1 兼容路径，且必须回放全部已读字节。
//
// 「仅 v2」入口或服务端关闭 v2 时，判定失败即拒绝：拒绝统一经过关闭出口，
// 不存在「既不成功也不关闭」的中间状态。
func (guard *ConnectionGuard) DetectVersion(source io.Reader) (Version, error) {
	peeker := NewPeekReader(source)
	prefix, err := peeker.Peek(len(V2Magic))

	if err != nil {
		return guard.detectFromShortPrefix(peeker, prefix, err)
	}
	guard.reader = peeker

	if bytes.Equal(prefix, V2Magic) {
		return guard.acceptV2(peeker)
	}
	return guard.acceptV1Fallback(peeker)
}

// detectFromShortPrefix 处理预读不足的情况：要么是空连接，要么是短 v1 帧。
func (guard *ConnectionGuard) detectFromShortPrefix(peeker *PeekReader, prefix []byte, cause error) (Version, error) {
	if len(prefix) == 0 {
		// 空连接既不算协议错误也不算成功：关闭连接但不发布拒绝事件。
		guard.closeWithEvent(CloseEvent{Stage: StageDetect, Succeeded: false})
		return "", ErrEmptyConnection
	}
	// 已读到部分字节即结束：不足魔数宽度说明不可能是 v2，按 v1 兼容路径回放。
	if cause == io.ErrUnexpectedEOF || cause == io.EOF {
		guard.reader = peeker
		return guard.acceptV1Fallback(peeker)
	}
	return "", guard.fail(protocolError(CategoryTransportFailure, StageDetect, "预读版本魔数失败"))
}

// acceptV2 在 v2 可用时消费魔数并选定 v2。
func (guard *ConnectionGuard) acceptV2(peeker *PeekReader) (Version, error) {
	if !guard.options.V2Enabled {
		return "", guard.fail(protocolError(CategoryVersionNotAccepted, StageDetect, "服务端已关闭 wire v2"))
	}
	if err := guard.LockVersion(VersionV2); err != nil {
		return "", guard.fail(err)
	}
	// 魔数是连接级前缀，不属于任何帧，必须在协商读取前消费掉。
	peeker.Consume(len(V2Magic))
	return VersionV2, nil
}

// acceptV1Fallback 在允许降级时回放字节并选定 v1。
func (guard *ConnectionGuard) acceptV1Fallback(peeker *PeekReader) (Version, error) {
	if guard.options.MaxWireVersion == VersionV2 {
		return "", guard.fail(protocolError(CategoryVersionNotAccepted, StageDetect, "该入口仅接受 wire v2"))
	}
	peeker.Replay()
	if err := guard.LockVersion(VersionV1); err != nil {
		return "", guard.fail(err)
	}
	// 回放后的流就是 v1 帧序列，此处把读取器接到同一预读流上。
	reader := NewV1Reader(peeker, DefaultV1PayloadLimit)
	reader.SetPool(guard.pool)
	guard.bindReader(reader)
	return VersionV1, nil
}
