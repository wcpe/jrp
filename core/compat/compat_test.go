package compat

import (
	"reflect"
	"testing"
)

func TestCurrentDescription(t *testing.T) {
	description := Current()

	if description.ProtocolID() != "jrp" {
		t.Fatalf("协议标识不匹配：%s", description.ProtocolID())
	}
	if description.ReferenceCommit() != "e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314" {
		t.Fatalf("参考提交不匹配：%s", description.ReferenceCommit())
	}
	if description.Baseline() != "jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314" {
		t.Fatalf("兼容基线不匹配：%s", description.Baseline())
	}

	assertStrings(t, "wire 版本", description.WireVersions(), []string{"v1", "v2"})
	assertStrings(t, "传输能力", description.Transports(), []string{"tcp", "kcp", "quic", "websocket", "wss"})
	assertStrings(t, "代理能力", description.ProxyTypes(), []string{"tcp", "udp", "http", "https", "stcp", "xtcp"})
}

func TestDescriptionCollectionsAreReadOnly(t *testing.T) {
	description := Current()
	wireVersions := description.WireVersions()
	transports := description.Transports()
	proxyTypes := description.ProxyTypes()

	wireVersions[0] = "已修改"
	transports[0] = "已修改"
	proxyTypes[0] = "已修改"

	assertStrings(t, "wire 版本", description.WireVersions(), []string{"v1", "v2"})
	assertStrings(t, "传输能力", description.Transports(), []string{"tcp", "kcp", "quic", "websocket", "wss"})
	assertStrings(t, "代理能力", description.ProxyTypes(), []string{"tcp", "udp", "http", "https", "stcp", "xtcp"})
}

func assertStrings(t *testing.T, name string, actual, expected []string) {
	t.Helper()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s不匹配：实际 %v，期望 %v", name, actual, expected)
	}
}
