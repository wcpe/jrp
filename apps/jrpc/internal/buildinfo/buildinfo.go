package buildinfo

import (
	"fmt"
	"strings"

	"github.com/wcpe/jrp/core/compat"
)

var Version = "0.1.0"

func VersionText(program string) string {
	description := compat.Current()
	return fmt.Sprintf(
		"%s %s\n协议 %s\nCore 基线 %s\nwire %s\n",
		program,
		Version,
		description.ProtocolID(),
		description.Baseline(),
		strings.Join(description.WireVersions(), ","),
	)
}
