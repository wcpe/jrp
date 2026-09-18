package wire

import (
	"sync"
	"testing"
)

// TestTransformStateAcceptsBothOrderings 断言两种启用顺序都能到达已加密＋已压缩。
func TestTransformStateAcceptsBothOrderings(t *testing.T) {
	negotiated := NegotiationResult{
		MessageCodec:         CodecJSON,
		CryptoAlgorithm:      CryptoStreamV1,
		CompressionAlgorithm: "jrp-gzip-v1",
		MaxPayload:           DefaultV2PayloadLimit,
	}

	t.Run("先加密后压缩", func(t *testing.T) {
		state := NewTransformState(negotiated)
		if err := state.EnableEncryption([]byte("jrp-test-key-material")); err != nil {
			t.Fatalf("启用加密失败：%v", err)
		}
		if err := state.EnableCompression(); err != nil {
			t.Fatalf("启用压缩失败：%v", err)
		}
		if !state.Encrypted() || !state.Compressed() {
			t.Fatal("未到达已加密＋已压缩")
		}
	})

	t.Run("先压缩后加密", func(t *testing.T) {
		state := NewTransformState(negotiated)
		if err := state.EnableCompression(); err != nil {
			t.Fatalf("启用压缩失败：%v", err)
		}
		if err := state.EnableEncryption([]byte("jrp-test-key-material")); err != nil {
			t.Fatalf("启用加密失败：%v", err)
		}
		if !state.Encrypted() || !state.Compressed() {
			t.Fatal("未到达已加密＋已压缩")
		}
	})
}

// TestTransformStateRejectsSixIllegalTransitions 覆盖六种非法迁移。
func TestTransformStateRejectsSixIllegalTransitions(t *testing.T) {
	negotiated := NegotiationResult{
		MessageCodec:         CodecJSON,
		CryptoAlgorithm:      CryptoStreamV1,
		CompressionAlgorithm: "jrp-gzip-v1",
		MaxPayload:           DefaultV2PayloadLimit,
	}
	withoutCompression := negotiated
	withoutCompression.CompressionAlgorithm = CompressionNone

	cases := []struct {
		name  string
		build func(negotiated NegotiationResult) *TransformState
		act   func(state *TransformState) error
	}{
		{
			name:  "重复启用加密",
			build: NewTransformState,
			act: func(state *TransformState) error {
				if err := state.EnableEncryption([]byte("key-material-0001")); err != nil {
					return err
				}
				return state.EnableEncryption([]byte("key-material-0002"))
			},
		},
		{
			name:  "重复启用压缩",
			build: NewTransformState,
			act: func(state *TransformState) error {
				if err := state.EnableCompression(); err != nil {
					return err
				}
				return state.EnableCompression()
			},
		},
		{
			name: "在未协商算法上启用加密",
			build: func(NegotiationResult) *TransformState {
				return NewTransformState(NegotiationResult{
					MessageCodec:         CodecJSON,
					CryptoAlgorithm:      CryptoNone,
					CompressionAlgorithm: CompressionNone,
					MaxPayload:           DefaultV2PayloadLimit,
				})
			},
			act: func(state *TransformState) error {
				return state.EnableEncryption([]byte("key-material-0001"))
			},
		},
		{
			name: "在未协商算法上启用压缩",
			build: func(NegotiationResult) *TransformState {
				return NewTransformState(withoutCompression)
			},
			act: func(state *TransformState) error {
				return state.EnableCompression()
			},
		},
		{
			name: "中途更换密钥参数",
			build: func(result NegotiationResult) *TransformState {
				state := NewTransformState(result)
				_ = state.EnableEncryption([]byte("key-material-0001"))
				return state
			},
			act: func(state *TransformState) error {
				return state.RotateKey()
			},
		},
		{
			name: "关闭已建立的加密",
			build: func(result NegotiationResult) *TransformState {
				state := NewTransformState(result)
				_ = state.EnableEncryption([]byte("key-material-0001"))
				return state
			},
			act: func(state *TransformState) error {
				return state.DisableEncryption()
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.act(testCase.build(negotiated))
			if err == nil {
				t.Fatal("非法迁移必须被拒绝")
			}
			if !IsCategory(err, CategoryTransformRejected) {
				t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
			}
		})
	}
}

// TestTransformStateRejectsApplicationWriteBeforeEncryption 断言加密前不得写入应用载荷。
func TestTransformStateRejectsApplicationWriteBeforeEncryption(t *testing.T) {
	negotiated := NegotiationResult{
		MessageCodec:         CodecJSON,
		CryptoAlgorithm:      CryptoStreamV1,
		CompressionAlgorithm: CompressionNone,
		MaxPayload:           DefaultV2PayloadLimit,
	}
	state := NewTransformState(negotiated)

	if err := state.WriteApplicationPayload([]byte("明文应用数据")); !IsCategory(err, CategoryTransformRejected) {
		t.Fatalf("加密启用前的应用写入必须被拒绝，实际 %v", err)
	}
	// 加密启用前明文应用数据不得进入连接，因此状态必须保持未加密。
	if state.Encrypted() {
		t.Fatal("写入被拒绝后不应改变加密状态")
	}

	if err := state.EnableEncryption([]byte("key-material-0001")); err != nil {
		t.Fatalf("启用加密失败：%v", err)
	}
	if err := state.WriteApplicationPayload([]byte("密文应用数据")); err != nil {
		t.Fatalf("加密启用后写入失败：%v", err)
	}
}

// TestTransformStateIsIndependentPerConnection 断言每条连接各自持有独立状态机。
func TestTransformStateIsIndependentPerConnection(t *testing.T) {
	negotiated := NegotiationResult{
		MessageCodec:         CodecJSON,
		CryptoAlgorithm:      CryptoStreamV1,
		CompressionAlgorithm: CompressionNone,
		MaxPayload:           DefaultV2PayloadLimit,
	}
	control := NewTransformState(negotiated)
	work := NewTransformState(negotiated)

	if err := control.EnableEncryption([]byte("control-key-material")); err != nil {
		t.Fatalf("控制连接启用加密失败：%v", err)
	}
	if work.Encrypted() {
		t.Fatal("控制连接的迁移影响了工作连接")
	}
}

// TestTransformStateConcurrentEnableRejectsSecondCall 断言并发启用只有一个成功。
func TestTransformStateConcurrentEnableRejectsSecondCall(t *testing.T) {
	negotiated := NegotiationResult{
		MessageCodec:         CodecJSON,
		CryptoAlgorithm:      CryptoStreamV1,
		CompressionAlgorithm: CompressionNone,
		MaxPayload:           DefaultV2PayloadLimit,
	}
	state := NewTransformState(negotiated)

	var waitGroup sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for index := 0; index < 8; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if err := state.EnableEncryption([]byte("key-material-0001")); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	waitGroup.Wait()

	if succeeded != 1 {
		t.Fatalf("并发启用必须恰好成功一次，实际 %d", succeeded)
	}
}
