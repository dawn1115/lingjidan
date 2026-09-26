//go:build windows

// console_windows.go 双击场景的「启动失败可见性」支持。
//
// 背景：Windows 下双击 exe 会新开一个控制台窗口，进程一退出窗口立即关闭。
// 启动阶段一旦失败（端口被占 / 配置写坏），用户只看到窗口一闪而过，报错内容
// 完全不可见——这是本项目在 Windows 分发时最高频的困惑来源。
//
// 处置：仅当判定「由双击启动」时，在致命退出前停住等按键。判定必须是**窄**的：
// 服务化运行、从 shell 启动、CI/脚本调用都不能被停住（否则会挂死无人值守的
// 部署流程），故用「当前控制台上只挂着自己一个进程」作为判据，见下。
package main

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// launchedByDoubleClick 报告进程是否由「双击」（资源管理器新开控制台）启动。
//
// 判据：存在控制台窗口，且挂在当前控制台上的进程只有自己。
//   - 双击：Windows 以 CREATE_NEW_CONSOLE 为本进程单独开控制台 → 只有自己 → true；
//   - cmd / PowerShell 启动：子进程附着在父 shell 既有的控制台上 → 至少 2 个 → false；
//   - 服务化 / 重定向输出 / 无控制台 → GetConsoleWindow 返回 0 → false。
//
// 任一 API 调用失败都返回 false（宁可不停，不可误停——误停会让服务/CI 挂死）。
func launchedByDoubleClick() bool {
	if w, _, _ := procGetConsoleWindow.Call(); w == 0 {
		return false // 无控制台：服务 / 后台 / 输出重定向
	}
	// 容量 8：控制台挂载进程多于 8 时 GetConsoleProcessList 返回实际所需数量
	// （> 8），不会等于 1，同样落到「不停」，语义安全。
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// waitForExitKey 停住等待用户按键，让控制台窗口不随进程退出立即关闭。
//
// LJD_NO_PAUSE 非空时直接返回（不额外输出）：自动化脚本、排障、批量启动等
// 场景需要与 log.Fatalf 完全一致的「立即退出」行为。
func waitForExitKey() {
	if os.Getenv("LJD_NO_PAUSE") != "" {
		return
	}
	fmt.Print("\n启动失败，窗口将保持打开。按回车键关闭…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
