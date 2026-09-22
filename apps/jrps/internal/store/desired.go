package store

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// DesiredSchemaVersion 是 desired 文档的当前 schema 版本。
//
// 版本锁定在文档根字段：解析时发现未知版本立即拒绝，不猜测兼容——真源内容
// 损坏或超前时，宁可让应用失败并暴露问题，也不能静默按错误配置重建监听。
const DesiredSchemaVersion = 1

// DefaultControlListenPort 是控制监听的默认端口。
//
// 控制监听是 desired 的一部分（FR-10 规格 §3.5：它不是引导参数，必须可无中断
// 热更），默认值与 docs/OPERATIONS.md 的示例端口一致。
const DefaultControlListenPort = 7200

// ControlListen 是 desired 文档中的控制监听设置。
type ControlListen struct {
	Host string
	Port int
}

// AddrPort 把监听设置转换为 Core 可接受的地址端口值。
func (listen ControlListen) AddrPort() (netip.AddrPort, error) {
	if listen.Port <= 0 || listen.Port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("控制监听端口越界：%d", listen.Port)
	}
	host := listen.Host
	if host == "" {
		host = "0.0.0.0"
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("控制监听地址非法 %q：%w", host, err)
	}
	return netip.AddrPortFrom(addr, uint16(listen.Port)), nil
}

// DesiredProxy 是文档中的单个代理条目。
//
// Deleted 是墓碑标记：删除动作把 Deleted 置真而不是移除条目，全量重建时
// 删除才不会在"缺条目=没变"与"缺条目=删了"之间产生歧义。
type DesiredProxy struct {
	ID             string `json:"id"`
	ClientID       string `json:"clientID"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	LocalPort      int    `json:"localPort,omitempty"`
	RemotePort     int    `json:"remotePort"`
	Target         string `json:"target,omitempty"`
	Transport      string `json:"transport,omitempty"`
	CaptureEnabled bool   `json:"captureEnabled,omitempty"`
	Deleted        bool   `json:"deleted,omitempty"`
}

// DesiredDocument 是 desired 内容的完整文档（schema v1）。
//
// 它是 ConfigRevision.Content 的唯一合法形态：SaveProxy/DeleteProxy 在同一
// 事务内从全量代理表重算生成，适配器据此构造 Core 快照。
type DesiredDocument struct {
	SchemaVersion int            `json:"schemaVersion"`
	ControlListen ControlListen  `json:"controlListen"`
	Proxies       []DesiredProxy `json:"proxies"`
}

// MarshalDesiredDocument 以紧凑 JSON 序列化文档并写入版本号。
func MarshalDesiredDocument(doc DesiredDocument) (string, error) {
	doc.SchemaVersion = DesiredSchemaVersion
	content, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("序列化 desired 文档失败：%w", err)
	}
	return string(content), nil
}

// ParseDesiredDocument 校验并解析 desired 内容。
//
// 校验在这里做一层（字段完整性、端口越界），构造 Core 快照时的类型化校验
// 由构建器再做一层（FR-32）；两层各管各的输入边界。
func ParseDesiredDocument(content string) (DesiredDocument, error) {
	var doc DesiredDocument
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return DesiredDocument{}, fmt.Errorf("desired 内容不是合法文档：%w", err)
	}
	if doc.SchemaVersion != DesiredSchemaVersion {
		return DesiredDocument{}, fmt.Errorf("desired 文档版本不支持：%d（当前 %d）", doc.SchemaVersion, DesiredSchemaVersion)
	}
	if doc.ControlListen.Port <= 0 || doc.ControlListen.Port > 65535 {
		return DesiredDocument{}, fmt.Errorf("控制监听端口越界：%d", doc.ControlListen.Port)
	}
	known := make(map[string]bool, len(doc.Proxies))
	names := make(map[string]bool, len(doc.Proxies))
	for index, proxy := range doc.Proxies {
		field := "proxies[" + strconv.Itoa(index) + "]"
		if proxy.ID == "" {
			return DesiredDocument{}, fmt.Errorf("%s 缺少代理标识", field)
		}
		if known[proxy.ID] {
			return DesiredDocument{}, fmt.Errorf("%s 代理标识重复：%s", field, proxy.ID)
		}
		known[proxy.ID] = true
		// 已删除条目是墓碑，不参与活动集合的校验。
		if proxy.Deleted {
			continue
		}
		if proxy.ClientID == "" {
			return DesiredDocument{}, fmt.Errorf("%s 缺少客户端标识", field)
		}
		if proxy.Name == "" {
			return DesiredDocument{}, fmt.Errorf("%s 缺少代理名称", field)
		}
		if names[proxy.Name] {
			return DesiredDocument{}, fmt.Errorf("%s 代理名称重复：%s", field, proxy.Name)
		}
		names[proxy.Name] = true
		if proxy.Type != "tcp" {
			return DesiredDocument{}, fmt.Errorf("%s 代理类型未支持：%s（P1 仅 tcp）", field, proxy.Type)
		}
		if proxy.RemotePort <= 0 || proxy.RemotePort > 65535 {
			return DesiredDocument{}, fmt.Errorf("%s 远程端口越界：%d", field, proxy.RemotePort)
		}
		if proxy.Target == "" {
			return DesiredDocument{}, fmt.Errorf("%s 缺少目标地址", field)
		}
		if _, err := netip.ParseAddrPort(proxy.Target); err != nil {
			return DesiredDocument{}, fmt.Errorf("%s 目标地址非法 %q：%w", field, proxy.Target, err)
		}
	}
	return doc, nil
}

// DesiredDocument 从事务内读取全量代理并生成完整 desired 文档。
//
// 它是 SaveProxy/DeleteProxy 追加版本时的内容来源：desired 是全量快照，
// 单代理的增量写入必须先重算全量再落库，保证内容与代理表永远一致。
func (tx *Tx) DesiredDocument() (DesiredDocument, error) {
	var proxies []Proxy
	if err := tx.db.Order("id ASC").Find(&proxies).Error; err != nil {
		return DesiredDocument{}, fmt.Errorf("读取代理列表失败：%w", translateSQLError(err))
	}
	doc := DesiredDocument{
		SchemaVersion: DesiredSchemaVersion,
		ControlListen: ControlListen{Host: "", Port: DefaultControlListenPort},
		Proxies:       make([]DesiredProxy, 0, len(proxies)),
	}
	for _, proxy := range proxies {
		doc.Proxies = append(doc.Proxies, DesiredProxy{
			ID:             proxy.ID,
			ClientID:       proxy.ClientID,
			Name:           proxy.Name,
			Type:           proxy.Type,
			LocalPort:      proxy.LocalPort,
			RemotePort:     proxy.RemotePort,
			Target:         proxy.Target,
			Transport:      proxy.Transport,
			CaptureEnabled: proxy.CaptureEnabled,
			Deleted:        proxy.Deleted,
		})
	}
	return doc, nil
}
