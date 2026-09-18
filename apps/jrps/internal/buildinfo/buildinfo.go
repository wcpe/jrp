package buildinfo

import (
	"fmt"
	"strings"

	"github.com/wcpe/jrp/core/compat"
)

var Version = "0.1.0"

type Info struct {
	Version      string
	ProtocolID   string
	CoreBaseline string
	WireVersions []string
}

func Current() Info {
	description := compat.Current()
	return Info{
		Version:      Version,
		ProtocolID:   description.ProtocolID(),
		CoreBaseline: description.Baseline(),
		WireVersions: description.WireVersions(),
	}
}

func VersionText(program string) string {
	info := Current()
	return fmt.Sprintf(
		"%s %s\n协议 %s\nCore 基线 %s\nwire %s\n",
		program,
		info.Version,
		info.ProtocolID,
		info.CoreBaseline,
		strings.Join(info.WireVersions, ","),
	)
}
