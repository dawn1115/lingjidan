package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fatalStartupEnv 子进程标记：命中即走 fatalStartup 分支。
const fatalStartupEnv = "LJD_TEST_FATAL_SUBPROCESS"

// TestFatalStartupExitContract fatalStartup 必须保住 log.Fatalf 的退出契约
// （记日志 + 退码 1），并且**在非双击场景不引入任何等待**。
//
// 风险点在被测代码的反面：若 launchedByDoubleClick 判定过宽，服务化 / CI /
// 脚本启动失败时会停在等按键上，把无人值守流程挂死。本测试在附着于既有控制台的
// 测试进程里运行（判据应落到 false），断言输出里**没有**出现等待提示。
// 子进程带超时，万一真挂住会以失败而非永久阻塞收场。
func TestFatalStartupExitContract(t *testing.T) {
	if os.Getenv(fatalStartupEnv) == "1" {
		fatalStartup("http: %v", errors.New("bind: address already in use"))
		return // 不可达：fatalStartup 必然 os.Exit
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestFatalStartupExitContract")
	cmd.Env = append(os.Environ(), fatalStartupEnv+"=1")
	// Stdin 缺省为 /dev/null：即便意外触发等待也立即 EOF，不会拖住测试。
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want exit error with code 1, got err=%v out=%s", err, out)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit code = %d want 1（保持 log.Fatalf 契约）", ee.ExitCode())
	}
	if !strings.Contains(string(out), "address already in use") {
		t.Errorf("失败原因必须写入日志（否则双击仍看不到）：%s", out)
	}
	if strings.Contains(string(out), "按回车键关闭窗口") {
		t.Errorf("非双击场景不得停住等按键（会挂死服务/CI）：%s", out)
	}
}

// TestWaitForExitKeyEscapeHatch LJD_NO_PAUSE=1 必须让等待立即返回且不额外输出，
// 使自动化场景与 log.Fatalf 行为完全一致。
func TestWaitForExitKeyEscapeHatch(t *testing.T) {
	t.Setenv("LJD_NO_PAUSE", "1")
	done := make(chan struct{})
	go func() {
		waitForExitKey()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LJD_NO_PAUSE=1 时 waitForExitKey 应立即返回")
	}
}
