//go:build !windows

// console_other.go 非 Windows 平台的空实现。
//
// 「双击后窗口一闪而过」是 Windows 控制台的特有现象：Unix 终端里进程退出后
// 终端（及其回滚缓冲）保留，报错照常可读，不存在本问题。故此处恒不停住，
// 也避免 Docker / systemd 等无人值守场景被误挂起。
package main

// launchedByDoubleClick 非 Windows 恒 false（无该语义）。
func launchedByDoubleClick() bool { return false }

// waitForExitKey 非 Windows 空实现。
func waitForExitKey() {}
