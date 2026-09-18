//go:build !webui

package webui

import (
	"embed"
	"io/fs"
)

//go:embed fallback
var content embed.FS

func FileSystem() fs.FS {
	assets, err := fs.Sub(content, "fallback")
	if err != nil {
		panic("无法加载内嵌 Web 回退资源")
	}
	return assets
}
