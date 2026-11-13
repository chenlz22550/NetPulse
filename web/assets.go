// Package web 打包管理网页的静态文件。
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static 返回 static 目录的文件系统。
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
