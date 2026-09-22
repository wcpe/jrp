package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// 文档编解码 round-trip：写入的字段读回后一致，未知字段被拒绝（schemaVersion 锁定）。
func TestDesiredDocumentRoundTrip(t *testing.T) {
	doc := DesiredDocument{
		SchemaVersion: DesiredSchemaVersion,
		ControlListen: ControlListen{Host: "127.0.0.1", Port: 7200},
		Proxies: []DesiredProxy{
			{ID: "p1", ClientID: "c1", Name: "ssh", Type: "tcp", LocalPort: 22, RemotePort: 6022, Target: "127.0.0.1:22", CaptureEnabled: false, Deleted: false},
		},
	}
	content, err := MarshalDesiredDocument(doc)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	parsed, err := ParseDesiredDocument(content)
	if err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	// Marshal 会写入版本号，所以读回的版本必为 1；其余字段逐项比对。
	if parsed.SchemaVersion != DesiredSchemaVersion || parsed.ControlListen != doc.ControlListen {
		t.Fatalf("round-trip 标头不一致：读回 %+v，写入 %+v", parsed, doc)
	}
	if len(parsed.Proxies) != len(doc.Proxies) {
		t.Fatalf("round-trip 代理数不一致：读回 %d，写入 %d", len(parsed.Proxies), len(doc.Proxies))
	}
	for index := range doc.Proxies {
		if parsed.Proxies[index] != doc.Proxies[index] {
			t.Fatalf("round-trip 代理 %d 不一致：读回 %+v，写入 %+v", index, parsed.Proxies[index], doc.Proxies[index])
		}
	}
	if !strings.Contains(content, `"schemaVersion":1`) {
		t.Fatalf("内容缺少 schemaVersion=1：%s", content)
	}
}

// 非法内容必须报错而不是静默为零值文档：真源内容损坏时宁可拒绝应用。
func TestParseDesiredDocumentRejectsGarbage(t *testing.T) {
	for name, content := range map[string]string{
		"非 JSON":     "这不是 JSON",
		"空文档":        "",
		"缺版本":        `{"controlListen":{"host":"127.0.0.1","port":7200},"proxies":[]}`,
		"版本超前":       `{"schemaVersion":2,"controlListen":{},"proxies":[]}`,
		"缺控制监听":      `{"schemaVersion":1,"proxies":[]}`,
		"端口越界":       `{"schemaVersion":1,"controlListen":{"host":"127.0.0.1","port":70000},"proxies":[]}`,
		"代理缺标识":      `{"schemaVersion":1,"controlListen":{"host":"h","port":1},"proxies":[{"clientID":"c1","name":"n","type":"tcp","remotePort":1}]}`,
		"代理远程端口越界":   `{"schemaVersion":1,"controlListen":{"host":"h","port":1},"proxies":[{"id":"p1","clientID":"c1","name":"n","type":"tcp","remotePort":0}]}`,
		"代理类型未支持":    `{"schemaVersion":1,"controlListen":{"host":"h","port":1},"proxies":[{"id":"p1","clientID":"c1","name":"n","type":"stcp","remotePort":1}]}`,
		"TCP 代理目标为空": `{"schemaVersion":1,"controlListen":{"host":"h","port":1},"proxies":[{"id":"p1","clientID":"c1","name":"n","type":"tcp","remotePort":1,"target":""}]}`,
	} {
		if _, err := ParseDesiredDocument(content); err == nil {
			t.Fatalf("%s 应被拒绝，实际通过", name)
		}
	}
}

// 已删除的代理保留在文档里（tombstone），确保 desired 全量重建时删除动作不丢失。
func TestDesiredDocumentKeepsTombstones(t *testing.T) {
	doc := DesiredDocument{
		ControlListen: ControlListen{Host: "0.0.0.0", Port: 7200},
		Proxies: []DesiredProxy{
			{ID: "p1", ClientID: "c1", Name: "ssh", Type: "tcp", RemotePort: 6022, Target: "127.0.0.1:22", Deleted: true},
		},
	}
	content, err := MarshalDesiredDocument(doc)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	parsed, err := ParseDesiredDocument(content)
	if err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if len(parsed.Proxies) != 1 || !parsed.Proxies[0].Deleted {
		t.Fatalf("删除标记未保留：%+v", parsed.Proxies)
	}
}

// DocumentSnapshot 从事务内读全量代理并生成完整文档：desired 是全量而不是增量。
func TestDesiredDocumentSnapshot(t *testing.T) {
	path := t.TempDir() + "/jrps.db"
	st := openServerStore(t, path)
	actor := ActorAdmin("admin")

	var revision uint64
	if err := st.Transaction(t.Context(), func(tx *Tx) error {
		var err error
		revision, err = tx.SaveProxy(Proxy{ID: "p1", ClientID: "c1", Name: "ssh", Type: "tcp", LocalPort: 22, RemotePort: 6022, Target: "127.0.0.1:22"}, actor, OriginProxyCreate)
		return err
	}); err != nil {
		t.Fatalf("写入代理失败：%v", err)
	}

	if err := st.View(t.Context(), func(tx *Tx) error {
		doc, err := tx.DesiredDocument()
		if err != nil {
			return err
		}
		if len(doc.Proxies) != 1 {
			t.Fatalf("快照应含 1 个代理，实际 %d 个", len(doc.Proxies))
		}
		proxy := doc.Proxies[0]
		if proxy.ID != "p1" || proxy.RemotePort != 6022 || proxy.Deleted {
			t.Fatalf("快照内容不符：%+v", proxy)
		}
		// 与最新 revision 的内容一致：同一次事务内读到的就是 desired 真源。
		latest, err := tx.LatestRevision()
		if err != nil {
			return err
		}
		if latest.Revision != revision {
			t.Fatalf("revision 不一致：快照来自 %d，最新 %d", revision, latest.Revision)
		}
		if _, err := ParseDesiredDocument(latest.Content); err != nil {
			t.Fatalf("落库内容不是合法文档：%v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("读取快照失败：%v", err)
	}
}

// 控制监听设置属于 desired 文档：默认值在无显式设置时落库为文档的一部分。
func TestDesiredDocumentDefaultControlListen(t *testing.T) {
	path := t.TempDir() + "/jrps.db"
	st := openServerStore(t, path)

	if err := st.View(t.Context(), func(tx *Tx) error {
		doc, err := tx.DesiredDocument()
		if err != nil {
			return err
		}
		if doc.ControlListen.Port == 0 {
			t.Fatalf("控制监听端口不应为 0：%+v", doc.ControlListen)
		}
		return nil
	}); err != nil {
		t.Fatalf("读取快照失败：%v", err)
	}
}

// Content 必须是紧凑 JSON（无缩进）：内容作为真源原样落库，保证字节级可比较。
func TestMarshalDesiredDocumentIsCompact(t *testing.T) {
	content, err := MarshalDesiredDocument(DesiredDocument{
		ControlListen: ControlListen{Host: "127.0.0.1", Port: 7200},
	})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if strings.ContainsAny(content, "\n\t ") && strings.Contains(content, ": ") {
		// json.Marshal 本身产出紧凑格式；若出现 ": " 说明实现误用了 MarshalIndent。
		t.Fatalf("内容不是紧凑 JSON：%s", content)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &probe); err != nil {
		t.Fatalf("内容不是合法 JSON：%v", err)
	}
}
