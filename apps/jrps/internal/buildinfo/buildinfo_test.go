package buildinfo

import (
	"reflect"
	"testing"
)

func TestCurrentUsesCoreCompatibilityBaseline(t *testing.T) {
	info := Current()

	if info.Version != "0.1.0" {
		t.Fatalf("默认版本不匹配：%s", info.Version)
	}
	if info.ProtocolID != "jrp" {
		t.Fatalf("协议标识不匹配：%s", info.ProtocolID)
	}
	if info.CoreBaseline != "jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314" {
		t.Fatalf("Core 基线不匹配：%s", info.CoreBaseline)
	}
	if !reflect.DeepEqual(info.WireVersions, []string{"v1", "v2"}) {
		t.Fatalf("wire 版本不匹配：%v", info.WireVersions)
	}
}

func TestVersionText(t *testing.T) {
	text := VersionText("jrps")
	if text != "jrps 0.1.0\n协议 jrp\nCore 基线 jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314\nwire v1,v2\n" {
		t.Fatalf("版本输出不匹配：%q", text)
	}
}
