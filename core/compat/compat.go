package compat

const (
	ProtocolID      = "jrp"
	ReferenceCommit = "e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314"
)

type Description struct {
	protocolID      string
	referenceCommit string
	wireVersions    []string
	transports      []string
	proxyTypes      []string
}

var current = Description{
	protocolID:      ProtocolID,
	referenceCommit: ReferenceCommit,
	wireVersions:    []string{"v1", "v2"},
	transports:      []string{"tcp", "kcp", "quic", "websocket", "wss"},
	proxyTypes:      []string{"tcp", "udp", "http", "https", "stcp", "xtcp"},
}

func Current() Description {
	return current
}

func (description Description) ProtocolID() string {
	return description.protocolID
}

func (description Description) ReferenceCommit() string {
	return description.referenceCommit
}

func (description Description) Baseline() string {
	return description.protocolID + "@" + description.referenceCommit
}

func (description Description) WireVersions() []string {
	return copyStrings(description.wireVersions)
}

func (description Description) Transports() []string {
	return copyStrings(description.transports)
}

func (description Description) ProxyTypes() []string {
	return copyStrings(description.proxyTypes)
}

func copyStrings(values []string) []string {
	return append([]string(nil), values...)
}
