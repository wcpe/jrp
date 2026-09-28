package wire

import (
	"bytes"
	"io"
	"testing"
)

// v1 控制通道加密的往返：写侧先发 IV 再发密文，读侧按同序消费并解密。
func TestV1CipherRoundTrip(t *testing.T) {
	key, err := V1ControlCipherKey("token-abc")
	if err != nil {
		t.Fatalf("派生密钥失败：%v", err)
	}
	if len(key) != 16 {
		t.Fatalf("密钥长度应为 16 字节（AES-128），实际 %d", len(key))
	}

	payloads := [][]byte{[]byte("第一条控制消息"), []byte("second"), {0x00, 0x01, 0xff, 0x7f}}
	var stream bytes.Buffer
	writer, err := NewV1CipherWriter(&stream, key)
	if err != nil {
		t.Fatalf("构造加密写入器失败：%v", err)
	}
	for _, payload := range payloads {
		if _, err := writer.Write(payload); err != nil {
			t.Fatalf("写入失败：%v", err)
		}
	}

	encoded := stream.Bytes()
	if len(encoded) < 16 {
		t.Fatalf("输出应包含 16 字节 IV，实际 %d 字节", len(encoded))
	}
	// 概率性检查：密文里不应出现明文片段（CFB 下重复出现的概率可忽略）。
	if bytes.Contains(encoded[16:], payloads[0]) {
		t.Fatal("链路输出包含明文片段，加密未生效")
	}

	reader := NewV1CipherReader(bytes.NewReader(encoded), key)
	for index, payload := range payloads {
		decrypted := make([]byte, len(payload))
		if _, err := io.ReadFull(reader, decrypted); err != nil {
			t.Fatalf("读取第 %d 段失败：%v", index, err)
		}
		if !bytes.Equal(decrypted, payload) {
			t.Fatalf("第 %d 段解密结果与原文不一致", index)
		}
	}
}

// 不同密钥不能解密彼此的流：错误密钥下解出的是噪声而不是原文。
func TestV1CipherRejectsForeignKey(t *testing.T) {
	key, err := V1ControlCipherKey("token-abc")
	if err != nil {
		t.Fatalf("派生密钥失败：%v", err)
	}
	foreignKey, err := V1ControlCipherKey("token-xyz")
	if err != nil {
		t.Fatalf("派生密钥失败：%v", err)
	}

	payload := []byte("控制通道载荷")
	var stream bytes.Buffer
	writer, err := NewV1CipherWriter(&stream, key)
	if err != nil {
		t.Fatalf("构造加密写入器失败：%v", err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	reader := NewV1CipherReader(bytes.NewReader(stream.Bytes()), foreignKey)
	decrypted := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, decrypted); err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if bytes.Equal(decrypted, payload) {
		t.Fatal("错误密钥不应解出原文")
	}
}

// 密钥派生是确定性的：同 token 派生出同密钥（官方协议要求两端一致）。
func TestV1ControlCipherKeyDeterministic(t *testing.T) {
	first, err := V1ControlCipherKey("same-token")
	if err != nil {
		t.Fatalf("派生密钥失败：%v", err)
	}
	second, err := V1ControlCipherKey("same-token")
	if err != nil {
		t.Fatalf("派生密钥失败：%v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("同 token 应派生出相同密钥")
	}
}
