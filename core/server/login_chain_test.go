package server

import (
	"strings"
	"testing"
	"time"
)

// tokenMismatch 拒绝：快照里存的是摘要，明文不匹配时登录失败。
func TestLoginChainRejectsTokenMismatch(t *testing.T) {
	digest := DigestToken("real-secret")
	gen := &generation{config: mustCredentialConfig(t, "c1", digest)}

	if matched, _ := gen.credentialsMatch("c1", "wrong-secret", 0); matched {
		t.Fatal("错误明文不应通过摘要比较")
	}
	if matched, _ := gen.credentialsMatch("c1", "", 0); matched {
		t.Fatal("空 token 不应通过")
	}
}

// 摘要匹配接受：客户端传明文，服务端摘要后与快照内的摘要恒定时间比较。
func TestLoginChainAcceptsDigestMatch(t *testing.T) {
	digest := DigestToken("real-secret")
	gen := &generation{config: mustCredentialConfig(t, "c1", digest)}

	if matched, _ := gen.credentialsMatch("c1", "real-secret", 0); !matched {
		t.Fatal("正确明文应通过摘要比较")
	}
}

// 官方链：官方 frpc 送 md5(token ∥ timestamp)，服务端用快照里的明文复算比对。
//
// 只有配置了兼容明文材料的客户端才走这条链；材料是官方协议的既定算法，
// 时间戳参与其中，因此同一 token 在不同时间戳下材料不同。
func TestLoginChainAcceptsOfficialPrivilegeKey(t *testing.T) {
	gen := &generation{config: mustCredentialConfigWithCompat(t, "c1", DigestToken("real-secret"), "real-secret")}
	timestamp := time.Now().Unix()

	official := officialPrivilegeKey("real-secret", timestamp)
	if matched, _ := gen.credentialsMatch("c1", official, timestamp); !matched {
		t.Fatal("官方鉴权材料应通过兼容链")
	}
	if matched, _ := gen.credentialsMatch("c1", official, timestamp+1); matched {
		t.Fatal("时间戳不匹配的官方材料不应通过")
	}
	if matched, _ := gen.credentialsMatch("c1", officialPrivilegeKey("other-secret", timestamp), timestamp); matched {
		t.Fatal("明文不匹配的官方材料不应通过")
	}
}

// 未知客户端拒绝。
func TestLoginChainRejectsUnknownClient(t *testing.T) {
	digest := DigestToken("real-secret")
	gen := &generation{config: mustCredentialConfig(t, "c1", digest)}

	if matched, _ := gen.credentialsMatch("ghost", DigestToken("real-secret"), 0); matched {
		t.Fatal("未知客户端不应通过")
	}
}

// 时间窗口：登录时间材料超出 ±窗口拒绝；窗口内通过。
func TestLoginChainTimeWindow(t *testing.T) {
	chain := newLoginChain(loginChainConfig{
		TimeWindow: 2 * time.Minute,
	})
	now := time.Now().UnixMilli()

	if err := chain.checkTimeWindow(now); err != nil {
		t.Fatalf("窗口内的材料不应被拒绝：%v", err)
	}
	stale := time.Now().Add(-3 * time.Minute).UnixMilli()
	if err := chain.checkTimeWindow(stale); err == nil {
		t.Fatal("过期材料应被拒绝")
	}
	future := time.Now().Add(5 * time.Minute).UnixMilli()
	if err := chain.checkTimeWindow(future); err == nil {
		t.Fatal("未来材料应被拒绝")
	}
}

// 重放边界：同一 runID+timestamp 只能成功登录一次；上限收敛防无界增长。
func TestLoginChainReplayBoundary(t *testing.T) {
	chain := newLoginChain(loginChainConfig{
		TimeWindow: 2 * time.Minute,
	})
	const runID = "run-1"
	ts := time.Now().UnixMilli()

	if err := chain.checkReplay(runID, ts); err != nil {
		t.Fatalf("首次提交不应被拒：%v", err)
	}
	if err := chain.checkReplay(runID, ts); err == nil {
		t.Fatal("同一 runID+时间戳的重复提交应判为重放")
	}
	// 新运行 ID 同一时间戳：允许（不同运行实例可能同毫秒启动）。
	if err := chain.checkReplay("run-2", ts); err != nil {
		t.Fatalf("新运行 ID 不应判为重放：%v", err)
	}
}

// 重放缓存有上限：超过上限时淘汰最旧条目，不无界增长。
func TestLoginChainReplayCacheBounded(t *testing.T) {
	chain := newLoginChain(loginChainConfig{
		TimeWindow:      2 * time.Minute,
		ReplayCacheSize: 8,
	})
	base := time.Now().UnixMilli()

	for i := 0; i < 16; i++ {
		if err := chain.checkReplay(string(rune('a'+i)), base+int64(i)); err != nil {
			t.Fatalf("第 %d 次提交不应被拒：%v", i, err)
		}
	}
	if len(chain.recent) > 8 {
		t.Fatalf("缓存应收敛到上限 8，实际 %d", len(chain.recent))
	}
}

// 稳定失败类别：错误文本只含类别与 clientID，不含 token/摘要/时间材料原文。
func TestLoginChainFailureCategoriesDoNotLeak(t *testing.T) {
	const secret = "super-secret-token-value"
	const digest = "dummy"
	digestOf := DigestToken(secret)
	_ = digest

	cases := []struct {
		name    string
		err     error
		wantCat string
	}{
		{"token 不匹配", ErrLoginTokenMismatch, "token"},
		{"时间窗外", ErrLoginTimeWindow, "time_window"},
		{"重放", ErrLoginReplay, "replay"},
		{"wire 能力不足", ErrLoginWireCapability, "wire_capability"},
	}
	for _, tc := range cases {
		text := tc.err.Error()
		if !strings.Contains(text, tc.wantCat) {
			t.Fatalf("%s 的错误文本应含稳定类别 %s：%s", tc.name, tc.wantCat, text)
		}
		if strings.Contains(text, secret) || strings.Contains(text, digestOf) {
			t.Fatalf("%s 的错误文本泄漏了材料：%s", tc.name, text)
		}
	}
}
