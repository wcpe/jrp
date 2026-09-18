package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenVectors 是 JRP 自有黄金帧向量文件的结构。
type goldenVectors struct {
	Note         string           `json:"note"`
	V2MagicHex   string           `json:"v2MagicHex"`
	MessageTypes []goldenType     `json:"messageTypes"`
	V1           []goldenV1Vector `json:"v1"`
	V2           []goldenV2Vector `json:"v2"`
	Boundary     goldenBoundary   `json:"boundary"`
}

type goldenType struct {
	Name   string `json:"name"`
	V1Byte string `json:"v1Byte"`
	V2ID   uint16 `json:"v2ID"`
	Source string `json:"source"`
}

type goldenV1Vector struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Payload string `json:"payload"`
	Hex     string `json:"hex"`
}

type goldenV2Vector struct {
	Name      string `json:"name"`
	FrameType uint16 `json:"frameType"`
	Message   string `json:"message"`
	V2ID      uint16 `json:"v2ID"`
	Payload   string `json:"payload"`
	Hex       string `json:"hex"`
}

type goldenBoundary struct {
	Note    string `json:"note"`
	V1Limit int    `json:"v1Limit"`
	V2Limit int    `json:"v2Limit"`
}

func loadGolden(t *testing.T) goldenVectors {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "frames.json"))
	if err != nil {
		t.Fatalf("读取黄金帧向量失败：%v", err)
	}
	var vectors goldenVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("解析黄金帧向量失败：%v", err)
	}
	if len(vectors.V1) == 0 || len(vectors.V2) == 0 {
		t.Fatal("黄金帧向量为空，禁止静默通过")
	}
	return vectors
}

// padPayload 构造长度恰为 size 字节的合法 JSON 对象文本。
// 固定前缀 `{"pad":"` 与后缀 `"}` 共 10 字节，因此 size 不得小于 10。
func padPayload(size int) []byte {
	const overhead = len(`{"pad":""}`)
	if size < overhead {
		panic("padPayload 长度不足以构造合法 JSON")
	}
	return []byte(`{"pad":"` + strings.Repeat("a", size-overhead) + `"}`)
}

// TestGoldenV1ByteLayer 断言 v1 帧的字段宽度与字节序。
func TestGoldenV1ByteLayer(t *testing.T) {
	for _, vector := range loadGolden(t).V1 {
		t.Run(vector.Name, func(t *testing.T) {
			want := mustHex(t, vector.Hex)
			messageType, ok := MessageTypeByName(vector.Message)
			if !ok {
				t.Fatalf("消息类型 %q 未登记", vector.Message)
			}

			frame := Frame{Type: messageType, Payload: []byte(vector.Payload)}
			encoded, err := EncodeV1Frame(frame)
			if err != nil {
				t.Fatalf("编码 v1 帧失败：%v", err)
			}
			if !bytes.Equal(encoded, want) {
				t.Fatalf("编码字节不一致：实际 %x，期望 %x", encoded, want)
			}

			// 字段宽度与字节序必须逐字节成立：类型 1 字节 + 长度 8 字节大端有符号。
			if len(want) < 9 {
				t.Fatalf("向量总长不足帧头：%d", len(want))
			}
			if encoded[0] != messageType.V1Byte {
				t.Fatalf("类型字节不匹配：实际 0x%02x，期望 0x%02x", encoded[0], messageType.V1Byte)
			}
			declared := int64(len(vector.Payload))
			if got := int64(uint64(encoded[1])<<56 | uint64(encoded[2])<<48 | uint64(encoded[3])<<40 |
				uint64(encoded[4])<<32 | uint64(encoded[5])<<24 | uint64(encoded[6])<<16 |
				uint64(encoded[7])<<8 | uint64(encoded[8])); got != declared {
				t.Fatalf("长度字段大端有符号解释不一致：实际 %d，期望 %d", got, declared)
			}

			decoded, err := DecodeV1Frame(want, DefaultV1PayloadLimit)
			if err != nil {
				t.Fatalf("解码 v1 帧失败：%v", err)
			}
			if decoded.Type.Name != messageType.Name {
				t.Fatalf("解码消息类型不一致：实际 %s，期望 %s", decoded.Type.Name, messageType.Name)
			}
			if string(decoded.Payload) != vector.Payload {
				t.Fatalf("解码载荷不一致：实际 %s", decoded.Payload)
			}
		})
	}
}

// TestGoldenV1RoundTripIsInvertible 断言编码与解码互为逆运算。
func TestGoldenV1RoundTripIsInvertible(t *testing.T) {
	for _, vector := range loadGolden(t).V1 {
		t.Run(vector.Name, func(t *testing.T) {
			messageType, ok := MessageTypeByName(vector.Message)
			if !ok {
				t.Fatalf("消息类型 %q 未登记", vector.Message)
			}
			original := Frame{Type: messageType, Payload: []byte(vector.Payload)}

			first, err := EncodeV1Frame(original)
			if err != nil {
				t.Fatalf("首次编码失败：%v", err)
			}
			decoded, err := DecodeV1Frame(first, DefaultV1PayloadLimit)
			if err != nil {
				t.Fatalf("解码失败：%v", err)
			}
			second, err := EncodeV1Frame(decoded)
			if err != nil {
				t.Fatalf("再次编码失败：%v", err)
			}
			if !bytes.Equal(first, second) {
				t.Fatalf("编解码不可重复：%x vs %x", first, second)
			}
		})
	}
}

// TestGoldenV2ByteLayer 断言 v2 帧头宽度与字节序，以及消息帧的类型 ID 前缀。
func TestGoldenV2ByteLayer(t *testing.T) {
	vectors := loadGolden(t)
	if got, want := mustHex(t, vectors.V2MagicHex), V2Magic[:]; !bytes.Equal(got, want) {
		t.Fatalf("v2 魔数不一致：实际 %x，期望 %x", got, want)
	}

	for _, vector := range vectors.V2 {
		t.Run(vector.Name, func(t *testing.T) {
			want := mustHex(t, vector.Hex)
			if len(want) < 8 {
				t.Fatalf("向量总长不足帧头：%d", len(want))
			}
			// 帧头固定 8 字节：类型 2 + flags 2 + 长度 4，全部网络字节序。
			if got := uint16(want[0])<<8 | uint16(want[1]); got != vector.FrameType {
				t.Fatalf("帧类型字段大端解释不一致：实际 %d，期望 %d", got, vector.FrameType)
			}
			if flags := uint16(want[2])<<8 | uint16(want[3]); flags != 0 {
				t.Fatalf("向量 flags 必须为 0，实际 %d", flags)
			}
			declared := int(uint32(want[4])<<24 | uint32(want[5])<<16 | uint32(want[6])<<8 | uint32(want[7]))
			if declared != len(want)-8 {
				t.Fatalf("长度字段大端解释不一致：实际 %d，期望 %d", declared, len(want)-8)
			}

			encoded, err := EncodeV2Frame(vector.FrameType, mustHex(t, vector.Hex)[8:])
			if err != nil {
				t.Fatalf("编码 v2 帧失败：%v", err)
			}
			if !bytes.Equal(encoded, want) {
				t.Fatalf("编码字节不一致：实际 %x，期望 %x", encoded, want)
			}

			frame, err := DecodeV2Frame(want, DefaultV2PayloadLimit)
			if err != nil {
				t.Fatalf("解码 v2 帧失败：%v", err)
			}
			if frame.FrameType != vector.FrameType {
				t.Fatalf("帧类型不一致：实际 %d", frame.FrameType)
			}
			if vector.FrameType != V2FrameTypeMessage {
				return
			}
			if frame.Message.Type.V2ID != vector.V2ID {
				t.Fatalf("消息类型 ID 不一致：实际 %d，期望 %d", frame.Message.Type.V2ID, vector.V2ID)
			}
			if string(frame.Message.Payload) != vector.Payload {
				t.Fatalf("消息载荷不一致：实际 %s", frame.Message.Payload)
			}
		})
	}
}

// TestGoldenMessageTypeTableMatchesVectors 断言消息类型表与向量一致。
func TestGoldenMessageTypeTableMatchesVectors(t *testing.T) {
	for _, entry := range loadGolden(t).MessageTypes {
		if len(entry.V1Byte) != 1 {
			t.Fatalf("向量中 v1 类型字节宽度不是 1：%q", entry.V1Byte)
		}
		byName, ok := MessageTypeByName(entry.Name)
		if !ok {
			t.Fatalf("消息类型 %q 未登记", entry.Name)
		}
		if byName.V1Byte != entry.V1Byte[0] {
			t.Fatalf("%s 的 v1 字节不一致：实际 0x%02x，期望 %q", entry.Name, byName.V1Byte, entry.V1Byte)
		}
		if byName.V2ID != entry.V2ID {
			t.Fatalf("%s 的 v2 ID 不一致：实际 %d，期望 %d", entry.Name, byName.V2ID, entry.V2ID)
		}
		if byV1, ok := MessageTypeByV1Byte(entry.V1Byte[0]); !ok || byV1.Name != entry.Name {
			t.Fatalf("按 v1 字节反查 %q 失败", entry.Name)
		}
		if byV2, ok := MessageTypeByV2ID(entry.V2ID); !ok || byV2.Name != entry.Name {
			t.Fatalf("按 v2 ID 反查 %q 失败", entry.Name)
		}
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("向量十六进制非法：%v", err)
	}
	return decoded
}
