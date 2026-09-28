package transport_test

import (
	"net"
	"testing"

	"github.com/wcpe/jrp/core/internal/transport"
)

// TestAddrSummaryMasksAddress 断言地址摘要不可逆推原值（规格 §3.6）。
//
// 迁移事件只承载摘要而非完整地址：事件用于观测路径变化，完整地址会把网络拓扑
// 带进事件流，既无必要也会泄露可定位信息。摘要必须满足两点——同一地址稳定
// （否则每次读写都误报一次迁移），不同地址可区分（否则无法判断"变化成什么"）。
func TestAddrSummaryMasksAddress(t *testing.T) {
	first, err := net.ResolveTCPAddr("tcp", "203.0.113.10:7200")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	second, err := net.ResolveTCPAddr("tcp", "203.0.113.11:7200")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}

	if summary := transport.AddrSummary(first); summary == first.String() {
		t.Fatalf("地址摘要原样返回了完整地址：%s", summary)
	}
	if summary := transport.AddrSummary(first); summary != transport.AddrSummary(first) {
		t.Fatalf("同一地址的摘要不稳定：%s 与 %s", summary, transport.AddrSummary(first))
	}
	if transport.AddrSummary(first) == transport.AddrSummary(second) {
		t.Fatalf("不同地址的摘要相同，无法区分迁移前后：%s", transport.AddrSummary(first))
	}
	if summary := transport.AddrSummary(nil); summary != "" {
		t.Fatalf("空地址应返回空摘要，实际：%s", summary)
	}
}

// TestAddrSummaryDistinguishesPorts 断言摘要能区分同一主机上的不同端口。
//
// 只掩码主机不掩码端口会把"同机不同端口"的迁移误判为无变化，事件失去意义。
func TestAddrSummaryDistinguishesPorts(t *testing.T) {
	first, err := net.ResolveTCPAddr("tcp", "203.0.113.10:7200")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	second, err := net.ResolveTCPAddr("tcp", "203.0.113.10:7201")
	if err != nil {
		t.Fatalf("解析地址失败：%v", err)
	}
	if transport.AddrSummary(first) == transport.AddrSummary(second) {
		t.Fatalf("不同端口的摘要相同：%s", transport.AddrSummary(first))
	}
}
