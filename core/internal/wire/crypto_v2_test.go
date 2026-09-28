package wire

import (
	"bytes"
	"io"
	"testing"
)

// v2 分帧 AEAD 的往返：写侧的流随机数 + 逐帧密文能被读侧还原。
func TestV2AEADRoundTrip(t *testing.T) {
	key, err := DeriveV2ControlKey("token-abc", V2CipherAlgorithmAES256GCM, V2DirectionClientToServer, []byte("transcript"))
	if err != nil {
		t.Fatalf("派生密钥失败：%v", err)
	}
	if len(key) != v2CipherKeySize {
		t.Fatalf("密钥长度应为 %d，实际 %d", v2CipherKeySize, len(key))
	}

	payloads := [][]byte{[]byte("第一条控制消息"), []byte("second"), {0x00, 0xff, 0x7f}}
	var stream bytes.Buffer
	writer, err := NewV2AEADWriter(&stream, key)
	if err != nil {
		t.Fatalf("构造写入器失败：%v", err)
	}
	for _, payload := range payloads {
		if _, err := writer.Write(payload); err != nil {
			t.Fatalf("写入失败：%v", err)
		}
	}
	encoded := stream.Bytes()
	if len(encoded) < 12 {
		t.Fatalf("输出应包含明文流随机数，实际 %d 字节", len(encoded))
	}
	if bytes.Contains(encoded[12:], payloads[0]) {
		t.Fatal("链路输出包含明文片段，加密未生效")
	}

	reader, err := NewV2AEADReader(bytes.NewReader(encoded), key)
	if err != nil {
		t.Fatalf("构造读取器失败：%v", err)
	}
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

// 不同方向派生出的密钥不同：协商双方必须按各自方向取密钥。
func TestV2ControlKeyDirectionsDiffer(t *testing.T) {
	toServer, err := DeriveV2ControlKey("token-abc", V2CipherAlgorithmAES256GCM, V2DirectionClientToServer, []byte("transcript"))
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}
	toClient, err := DeriveV2ControlKey("token-abc", V2CipherAlgorithmAES256GCM, V2DirectionServerToClient, []byte("transcript"))
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}
	if bytes.Equal(toServer, toClient) {
		t.Fatal("两个方向不应派生出相同密钥")
	}
}

// 协商记录参与密钥派生：任一段载荷变化都会得到不同密钥。
func TestV2ControlKeyBindsTranscript(t *testing.T) {
	first, err := DeriveV2ControlKey("token", V2CipherAlgorithmAES256GCM, V2DirectionClientToServer,
		V2CryptoTranscript([]byte("hello-a"), []byte("hello-b")))
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}
	second, err := DeriveV2ControlKey("token", V2CipherAlgorithmAES256GCM, V2DirectionClientToServer,
		V2CryptoTranscript([]byte("hello-a"), []byte("hello-c")))
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("协商记录变化应导致密钥变化")
	}
}

// 错误密钥下首帧认证失败：这是篡改与密钥不一致的统一表现。
func TestV2AEADRejectsForeignKey(t *testing.T) {
	key, err := DeriveV2ControlKey("token-abc", V2CipherAlgorithmAES256GCM, V2DirectionClientToServer, []byte("transcript"))
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}
	foreignKey, err := DeriveV2ControlKey("token-xyz", V2CipherAlgorithmAES256GCM, V2DirectionClientToServer, []byte("transcript"))
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}

	var stream bytes.Buffer
	writer, err := NewV2AEADWriter(&stream, key)
	if err != nil {
		t.Fatalf("构造写入器失败：%v", err)
	}
	if _, err := writer.Write([]byte("载荷")); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	reader, err := NewV2AEADReader(bytes.NewReader(stream.Bytes()), foreignKey)
	if err != nil {
		t.Fatalf("构造读取器失败：%v", err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 6)); err == nil {
		t.Fatal("错误密钥不应通过认证")
	}
}
