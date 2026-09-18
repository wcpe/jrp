//go:build webui

package webui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var content embed.FS

func FileSystem() fs.FS {
	assets, err := fs.Sub(content, "dist")
	if err != nil {
		panic("无法加载内嵌 Web 生产资源")
	}
	return assets
}
