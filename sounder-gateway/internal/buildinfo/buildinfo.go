// Package buildinfo 保存由 -ldflags 注入的构建信息。
package buildinfo

var (
	// Version 是发布版本（git tag 或 dev）。
	Version = "dev"
	// Commit 是构建时的 git commit。
	Commit = ""
)

// String 返回适合日志/接口展示的版本串。
func String() string {
	if Commit == "" {
		return Version
	}
	short := Commit
	if len(short) > 7 {
		short = short[:7]
	}
	return Version + "+" + short
}
