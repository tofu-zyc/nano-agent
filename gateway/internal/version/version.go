// Package version 保存编译时写进去的版本号。
package version

// Version 由 make 用 -ldflags -X 写入；直接 go build 时是 dev。
var Version = "dev"
