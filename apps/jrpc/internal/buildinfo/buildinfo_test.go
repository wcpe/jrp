package buildinfo

import "testing"

func TestVersionTextUsesCoreCompatibilityBaseline(t *testing.T) {
	text := VersionText("jrpc")
	if text != "jrpc 0.1.0\n协议 jrp\nCore 基线 jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314\nwire v1,v2\n" {
		t.Fatalf("版本输出不匹配：%q", text)
	}
}
