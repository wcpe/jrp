package wire

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"io"
)

// wire v1 控制通道加密（官方兼容）。
//
// 官方 frpc 在登录握手成功后把控制连接切换为加密通道：双向字节流各自先发 16 字节
// 随机 IV，其后是 AES-128-CFB 密文；密钥由 token 明文经 PBKDF2(SHA-1, 盐 "crypto",
// 64 次) 派生。登录握手本身是明文，切换点固定在登录响应写出之后，两侧顺序一致。
//
// 这里复刻的是官方线上算法，不是 JRP 自身的密码学选择：AES-128-CFB 与 SHA-1 只
// 服务于该兼容通道，不派生 JRP 内部的任何密钥或状态。
const (
	// v1CipherSalt 是官方实际使用的密钥派生盐值。
	//
	// 它不是 golib 的默认值 "crypto"：官方 frpc/frps 在启动时把 golib 的
	// DefaultSalt 覆盖为 "frp"（参考实现 client/service.go 与 server/service.go
	// 的初始化段）。黑盒取证确认两侧派生密钥在 salt="frp" 下逐字节一致，
	// 而默认盐下完全不同——这是该通道能互通的唯一前提。
	v1CipherSalt = "frp"
	// v1CipherIterations 是官方固定的 PBKDF2 迭代次数。
	v1CipherIterations = 64
)

// V1ControlCipherKey 派生 wire v1 控制通道的 AES-128 密钥。
//
// 口令是 token 明文：官方客户端与服务端都用它派生，因此持有明文是这条通道的
// 前提（与鉴权兼容例外同源）。
func V1ControlCipherKey(token string) ([]byte, error) {
	key, err := pbkdf2.Key(sha1.New, token, []byte(v1CipherSalt), v1CipherIterations, aes.BlockSize) //nolint:gosec // 复刻官方协议的线上算法
	if err != nil {
		return nil, fmt.Errorf("派生控制通道密钥失败：%w", err)
	}
	return key, nil
}

// NewV1CipherReader 包装入站字节流：首次读取时消费对端 16 字节 IV，其后解密。
//
// 每次读取就地解密返回的字节，流位置保持连续——CFB 是流密码，读取必须按序进行。
func NewV1CipherReader(source io.Reader, key []byte) io.Reader {
	return &v1CipherReader{source: source, key: key}
}

// NewV1CipherWriter 包装出站字节流：首次写入时先发送本端 16 字节随机 IV，其后加密。
func NewV1CipherWriter(target io.Writer, key []byte) (io.Writer, error) {
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("控制通道密钥长度非法：%d", len(key))
	}
	return &v1CipherWriter{target: target, key: key}, nil
}

// v1CipherReader 解密 v1 控制通道的入站字节流。
type v1CipherReader struct {
	source io.Reader
	key    []byte
	stream cipher.Stream
}

// Read 读取并就地解密入站字节。
func (reader *v1CipherReader) Read(target []byte) (int, error) {
	if reader.stream == nil {
		if err := reader.initialize(); err != nil {
			return 0, err
		}
	}
	count, err := reader.source.Read(target)
	if count > 0 {
		reader.stream.XORKeyStream(target[:count], target[:count])
	}
	return count, err
}

// initialize 消费对端 IV 并建立解密流。
func (reader *v1CipherReader) initialize() error {
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(reader.source, iv); err != nil {
		return fmt.Errorf("读取控制通道 IV 失败：%w", err)
	}
	block, err := aes.NewCipher(reader.key)
	if err != nil {
		return err
	}
	// CFB 是官方协议的既定算法：本模块复刻线上行为，不是 JRP 的密码学选型。
	reader.stream = cipher.NewCFBDecrypter(block, iv) //nolint:staticcheck // 兼容官方协议
	return nil
}

// v1CipherWriter 加密 v1 控制通道的出站字节流。
type v1CipherWriter struct {
	target io.Writer
	key    []byte
	stream cipher.Stream
}

// Write 加密并写出字节；首次写入前先发送本端 IV。
func (writer *v1CipherWriter) Write(payload []byte) (int, error) {
	if writer.stream == nil {
		if err := writer.initialize(); err != nil {
			return 0, err
		}
	}
	encrypted := make([]byte, len(payload))
	writer.stream.XORKeyStream(encrypted, payload)
	count, err := writer.target.Write(encrypted)
	if err != nil {
		return count, err
	}
	// 对端按明文长度推进流位置：这里回报明文长度，而不是密文字节数。
	return len(payload), nil
}

// initialize 生成并发送本端 IV，随后建立加密流。
func (writer *v1CipherWriter) initialize() error {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return fmt.Errorf("生成控制通道 IV 失败：%w", err)
	}
	if _, err := writer.target.Write(iv); err != nil {
		return fmt.Errorf("发送控制通道 IV 失败：%w", err)
	}
	block, err := aes.NewCipher(writer.key)
	if err != nil {
		return err
	}
	writer.stream = cipher.NewCFBEncrypter(block, iv) //nolint:staticcheck // 兼容官方协议
	return nil
}
