package wire

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// wire v2 控制通道加密（官方兼容）。
//
// v2 的加密不是可选包装而是协商结果的一部分：客户端在 hello 里声明支持的算法
// 集合，服务端选定其一并在 server hello 里回写，双方据此派生控制通道密钥并对其
// 后的所有控制消息启用分帧 AEAD。协商记录（两个 hello 的原始载荷）参与密钥派生，
// 因此任何一方篡改 hello 都会导致密钥不一致、首帧认证失败。
//
// 这里只实现 aes-256-gcm（标准库即可）：官方客户端同时声明 aes-256-gcm 与
// xchacha20-poly1305，服务端从中选定前者即可完成协商，Core 因此不需要引入
// 任何第三方依赖。
const (
	// V2CipherAlgorithmAES256GCM 是 JRP 在 v2 协商中选定的算法。
	V2CipherAlgorithmAES256GCM = "aes-256-gcm"
	// v2CipherKeySize 是协商与派生约定使用的密钥长度。
	v2CipherKeySize = 32
	// v2CipherMaxPayloadSize 是单帧明文上限（官方默认值）。
	v2CipherMaxPayloadSize = 64 * 1024
	// v2TranscriptLabel 是协商记录哈希的固定标签。
	//
	// 它与下面的 info 前缀是两个不同的常量：黑盒取证确认二者取值不同，
	// 混用会让派生密钥与对端不一致，且症状只在首帧认证时暴露。
	v2TranscriptLabel = "frp wire v2 crypto transcript"
	// v2ControlInfoPrefix 是控制通道密钥派生的 HKDF info 前缀。
	v2ControlInfoPrefix = "frp wire v2 control aead"
	// v2FrameHeaderSize 是每帧密文长度头的宽度。
	v2FrameHeaderSize = 4
	// v2CipherRandomSize 是协商随机材料的固定长度（官方校验要求）。
	v2CipherRandomSize = 32
)

// v2 控制通道密钥派生的两个方向标识（官方固定取值）。
const (
	V2DirectionClientToServer = "client-to-server"
	V2DirectionServerToClient = "server-to-client"
)

// V2CryptoTranscript 复算 v2 协商记录哈希。
//
// 输入是两个 hello 帧的原始载荷（未解密的字节），格式为：固定标签后跟两段
// 「零字节 + 段名 + 零字节 + 8 字节大端长度 + 载荷」。顺序与长度都参与哈希，
// 双方必须按同一顺序喂入。
func V2CryptoTranscript(clientHelloPayload, serverHelloPayload []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(v2TranscriptLabel))
	writeV2TranscriptPart(hash, "client hello", clientHelloPayload)
	writeV2TranscriptPart(hash, "server hello", serverHelloPayload)
	return hash.Sum(nil)
}

// writeV2TranscriptPart 写入协商记录的一段。
func writeV2TranscriptPart(hash io.Writer, label string, payload []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(label))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(payload)
}

// DeriveV2ControlKey 派生 v2 控制通道在一个方向上的 AEAD 密钥。
//
// 口令是 token 明文（与 v1 通道同源），盐是协商记录哈希，info 由固定前缀、
// 算法名与方向拼成；三个输入中任意一个不一致都会导致首帧认证失败。
func DeriveV2ControlKey(token, algorithm, direction string, transcript []byte) ([]byte, error) {
	info := v2ControlInfoPrefix + " " + algorithm + " " + direction
	key, err := hkdf.Key(sha256.New, []byte(token), transcript, info, v2CipherKeySize)
	if err != nil {
		return nil, fmt.Errorf("派生 v2 控制通道密钥失败：%w", err)
	}
	return key, nil
}

// NewV2AEADWriter 构造分帧 AEAD 写入器。
//
// 线格式：首帧之前先发送明文流随机数（nonce 长度），其后是重复的
// `uint32 密文长度 || 密文与认证标签`。每帧以流随机数与帧长度头作为附加认证
// 数据，帧随机数随帧号递增，因此帧顺序与内容一同被认证。
func NewV2AEADWriter(target io.Writer, key []byte) (io.Writer, error) {
	aead, err := newV2AEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成 v2 流随机数失败：%w", err)
	}
	return &v2AEADWriter{
		target:      target,
		aead:        aead,
		streamNonce: append([]byte(nil), nonce...),
		nonce:       nonce,
	}, nil
}

// NewV2AEADReader 构造分帧 AEAD 读取器；首次读取时消费对端明文流随机数。
func NewV2AEADReader(source io.Reader, key []byte) (io.Reader, error) {
	aead, err := newV2AEAD(key)
	if err != nil {
		return nil, err
	}
	return &v2AEADReader{source: source, aead: aead}, nil
}

// newV2AEAD 按算法构造 AEAD；当前只支持 aes-256-gcm。
func newV2AEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != v2CipherKeySize {
		return nil, fmt.Errorf("v2 控制通道密钥长度非法：%d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// v2AEADWriter 加密 v2 控制通道的出站字节流。
type v2AEADWriter struct {
	target      io.Writer
	aead        cipher.AEAD
	streamNonce []byte
	nonce       []byte
	headerSent  bool
}

// Write 按帧切分并写出密文。
func (writer *v2AEADWriter) Write(payload []byte) (int, error) {
	written := 0
	for len(payload) > 0 {
		chunk := payload
		if len(chunk) > v2CipherMaxPayloadSize {
			chunk = chunk[:v2CipherMaxPayloadSize]
		}
		if err := writer.writeFrame(chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		payload = payload[len(chunk):]
	}
	return written, nil
}

// writeFrame 写出单帧：长度头 + 密文，随机数逐帧递增。
func (writer *v2AEADWriter) writeFrame(plaintext []byte) error {
	if !writer.headerSent {
		if _, err := writer.target.Write(writer.streamNonce); err != nil {
			return fmt.Errorf("发送 v2 流随机数失败：%w", err)
		}
		writer.headerSent = true
	}
	var header [v2FrameHeaderSize]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(plaintext)+writer.aead.Overhead()))

	frame := make([]byte, 0, v2FrameHeaderSize+len(plaintext)+writer.aead.Overhead())
	frame = append(frame, header[:]...)
	frame = writer.aead.Seal(frame, writer.nonce, plaintext, writer.frameAAD(header[:]))
	incrementV2Nonce(writer.nonce)
	if _, err := writer.target.Write(frame); err != nil {
		return err
	}
	return nil
}

// frameAAD 组装帧的附加认证数据：流随机数与帧长度头。
func (writer *v2AEADWriter) frameAAD(header []byte) []byte {
	aad := make([]byte, 0, len(writer.streamNonce)+len(header))
	aad = append(aad, writer.streamNonce...)
	aad = append(aad, header...)
	return aad
}

// v2AEADReader 解密 v2 控制通道的入站字节流。
type v2AEADReader struct {
	source      io.Reader
	aead        cipher.AEAD
	streamNonce []byte
	nonce       []byte
	pending     []byte
	// frameCount 记录已成功解出的帧数，仅用于诊断。
	frameCount uint64
}

// Read 按帧解密；帧边界上的 EOF 视为正常结束。
func (reader *v2AEADReader) Read(target []byte) (int, error) {
	for len(reader.pending) == 0 {
		if err := reader.readFrame(); err != nil {
			return 0, err
		}
	}
	count := copy(target, reader.pending)
	reader.pending = reader.pending[count:]
	return count, nil
}

// readFrame 读取并认证一帧，明文留在 pending。
func (reader *v2AEADReader) readFrame() error {
	if reader.nonce == nil {
		nonce := make([]byte, reader.aead.NonceSize())
		if read, err := io.ReadFull(reader.source, nonce); err != nil {
			// 已读字节数编入错误：0 表示对端一个字节都没发出，介于两者之间表示
			// 流在流随机数中途结束——两种情况的排查方向完全不同。
			return fmt.Errorf("读取 v2 流随机数失败（已读 %d/%d 字节）：%w", read, len(nonce), err)
		}
		reader.streamNonce = nonce
		reader.nonce = append([]byte(nil), nonce...)
	}
	var header [v2FrameHeaderSize]byte
	if read, err := io.ReadFull(reader.source, header[:]); err != nil {
		return fmt.Errorf("读取 v2 帧头失败（已读 %d/%d 字节）：%w", read, len(header), err)
	}
	declared := int(binary.BigEndian.Uint32(header[:]))
	if declared < reader.aead.Overhead() || declared > v2CipherMaxPayloadSize+reader.aead.Overhead() {
		return fmt.Errorf("v2 帧密文长度非法：%d", declared)
	}
	ciphertext := make([]byte, declared)
	if read, err := io.ReadFull(reader.source, ciphertext); err != nil {
		return fmt.Errorf("读取 v2 密文失败（已读 %d/%d 字节）：%w", read, len(ciphertext), err)
	}
	aad := make([]byte, 0, len(reader.streamNonce)+len(header))
	aad = append(aad, reader.streamNonce...)
	aad = append(aad, header[:]...)

	plaintext, err := reader.aead.Open(nil, reader.nonce, ciphertext, aad)
	if err != nil {
		// 流随机数是双方对齐状态的直接指纹：认证失败时带上它，便于与对端记录
		// 的实际字节序列对照，判断是密钥不一致还是流位置漂移。
		return fmt.Errorf("v2 帧认证失败（流随机数=%x 帧号=%d 帧头=%x 声明长度=%d）：%w",
			reader.streamNonce, reader.frameCount, header, declared, err)
	}
	incrementV2Nonce(reader.nonce)
	reader.frameCount++
	reader.pending = plaintext
	return nil
}

// incrementV2Nonce 就地把帧随机数加一（大端进位，与官方实现一致）。
func incrementV2Nonce(nonce []byte) {
	for index := len(nonce) - 1; index >= 0; index-- {
		nonce[index]++
		if nonce[index] != 0 {
			return
		}
	}
}
