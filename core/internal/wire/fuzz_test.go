package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// FuzzV1Frame 用畸形输入语料驱动 wire v1 解析器。
//
// 断言：无 panic、无无界分配、无死锁。超限声明必须在分配之前被拒绝。
func FuzzV1Frame(f *testing.F) {
	seeds := [][]byte{
		mustHexF(f, loadGoldenF(f).V1[0].Hex),
		{MessageTypePing.V1Byte, 0, 0, 0, 0, 0, 0, 0, 0},
		{0xff, 'x'},
		nil,
		make([]byte, 32),
		bytes.Repeat([]byte{0xff}, 64),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<16 {
			t.Skip("输入过大，跳过")
		}
		frame, err := DecodeV1Frame(raw, DefaultV1PayloadLimit)
		if err != nil {
			if CategoryOf(err) == "" {
				t.Fatalf("错误必须携带稳定类别：%v", err)
			}
			return
		}
		// 解码成功必须能原样重新编码（载荷是合法 JSON 对象的前提下）。
		reencoded, err := EncodeV1FrameWithLimit(frame, DefaultV1PayloadLimit)
		if err != nil {
			t.Fatalf("成功解码的帧必须能重新编码：%v", err)
		}
		if len(reencoded) != V1HeaderSize+len(frame.Payload) {
			t.Fatalf("重新编码长度不一致：%d", len(reencoded))
		}
	})
}

// FuzzV2Frame 用畸形输入语料驱动 wire v2 解析器。
func FuzzV2Frame(f *testing.F) {
	seeds := [][]byte{
		{0, 1, 0, 0, 0, 0, 0, 0},
		{0, 16, 0, 0, 0, 0, 0, 2, 0, 1, '{', '}'},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		nil,
		make([]byte, 16),
		bytes.Repeat([]byte{0xff}, 64),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<16 {
			t.Skip("输入过大，跳过")
		}
		frame, err := DecodeV2Frame(raw, DefaultV2PayloadLimit)
		if err != nil {
			if CategoryOf(err) == "" {
				t.Fatalf("错误必须携带稳定类别：%v", err)
			}
			return
		}
		if frame.Flags != 0 {
			t.Fatalf("成功解码的帧不得带非零 flags：%d", frame.Flags)
		}
	})
}

// FuzzVersionDetection 用畸形前导驱动版本判定，断言判定结果稳定且不悬挂。
func FuzzVersionDetection(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{'o'})
	f.Add([]byte("FRP"))
	f.Add(append([]byte(nil), V2Magic...))
	f.Add(bytes.Repeat([]byte{0x00}, 16))
	f.Add(bytes.Repeat([]byte{0xff}, 16))

	f.Fuzz(func(t *testing.T, prefix []byte) {
		if len(prefix) > 4096 {
			t.Skip("输入过大，跳过")
		}
		guard := NewConnectionGuard(nil, nil, Options{MaxWireVersion: VersionV1, V2Enabled: true})

		done := make(chan error, 1)
		go func() {
			done <- func() error {
				_, err := guard.DetectVersion(bytes.NewReader(prefix))
				return err
			}()
		}()

		select {
		case err := <-done:
			if err != nil && CategoryOf(err) == CategoryVersionNotAccepted {
				return
			}
			if err != nil && !bytes.Equal(prefix, nil) {
				// 版本判定失败必须给出可判定类别或空连接语义。
				if err == ErrEmptyConnection {
					return
				}
				if CategoryOf(err) == "" {
					t.Fatalf("判定失败必须携带稳定类别：%v", err)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("版本判定悬挂")
		}
	})
}

// TestNoGoroutineLeakOnRejectedConnection 断言拒绝路径不泄漏 goroutine。
func TestNoGoroutineLeakOnRejectedConnection(t *testing.T) {
	before := runtime.NumGoroutine()

	var waitGroup sync.WaitGroup
	for index := 0; index < 32; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			guard := NewConnectionGuard(nil, nil, Options{MaxWireVersion: VersionV2, V2Enabled: true})
			_, _ = guard.DetectVersion(bytes.NewReader([]byte{'o', 0, 0, 0, 0, 0, 0, 0, 1, '{'}))
		}()
	}
	waitGroup.Wait()

	// 给运行时一点时间回收已退出的 goroutine。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("拒绝路径疑似泄漏 goroutine：之前 %d，之后 %d", before, runtime.NumGoroutine())
}

// TestDecodeDoesNotAllocatePerDeclaredLength 断言超限声明不触发按声明长度的分配。
func TestDecodeDoesNotAllocatePerDeclaredLength(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "v1 声明 1 GiB", raw: v1HeaderWithLength(1 << 30)},
		{name: "v2 声明 1 GiB", raw: v2HeaderWithLength(1 << 30)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			start := allocationSnapshot()
			if testCase.raw[0] == MessageTypePing.V1Byte {
				_, _ = DecodeV1Frame(testCase.raw, DefaultV1PayloadLimit)
			} else {
				_, _ = DecodeV2Frame(testCase.raw, DefaultV2PayloadLimit)
			}
			grew := allocationSnapshot() - start
			if grew > 1<<20 {
				t.Fatalf("超限声明触发了无界分配：增长 %d 字节", grew)
			}
		})
	}
}

func v1HeaderWithLength(length int64) []byte {
	raw := make([]byte, V1HeaderSize)
	raw[0] = MessageTypePing.V1Byte
	for index := 0; index < V1LengthFieldSize; index++ {
		shift := uint((V1LengthFieldSize - 1 - index) * 8)
		raw[1+index] = byte(uint64(length) >> shift)
	}
	return raw
}

func v2HeaderWithLength(length uint32) []byte {
	raw := make([]byte, V2HeaderSize)
	raw[1] = byte(V2FrameTypeMessage)
	raw[4] = byte(length >> 24)
	raw[5] = byte(length >> 16)
	raw[6] = byte(length >> 8)
	raw[7] = byte(length)
	return raw
}

func loadGoldenF(f *testing.F) goldenVectors {
	f.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "frames.json"))
	if err != nil {
		f.Fatalf("读取黄金帧向量失败：%v", err)
	}
	var vectors goldenVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		f.Fatalf("解析黄金帧向量失败：%v", err)
	}
	return vectors
}

func mustHexF(f *testing.F, value string) []byte {
	f.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		f.Fatalf("向量十六进制非法：%v", err)
	}
	return decoded
}
