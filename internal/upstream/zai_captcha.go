// zai_captcha.go Z.ai（Start Plan 免费额度）通道所需的阿里云无痕验证参数求解。
//
// 背景：zcode.z.ai 的 coding-plan 代理端点对 JWT（免费/套餐）通道的**每个**模型请求
// 都要求 X-Aliyun-Captcha-Verify-Param。该参数由阿里云无痕 SDK 现场签发，有效期短
// （实测 45s 内可复用、超时后失效）。缺失 → 上游 400 code 3007「captcha verify failed」。
//
// 本项目不内置浏览器，改为子进程调用 Node 求解器（captcha_node/solver.js，
// 用 happy-dom 模拟浏览器环境直接跑阿里云官方 SDK）。设计对齐社区验证实现
// （dengyie/zcode2api 的 app/captcha.py）：
//   - 结果缓存（默认 45s）：TTL 内复用同一参数，避免每请求都起进程
//   - 并发去重：同一时刻只跑一个求解进程，其余请求等锁后命中缓存
//   - 失败重试：单次求解偶发失败时自动重试
//
// 使用前提：在 captcha_node 目录执行过一次 npm install（安装 happy-dom）。
package upstream

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Z.ai 验证码参数默认值（与官方 client/configs 实测一致）。
const (
	zaiCaptchaScene  = "11xygtvd"
	zaiCaptchaPrefix = "no8xfe"
	zaiCaptchaRegion = "cn"
)

// zaiCaptchaTTL 求得的验证码参数缓存时长。社区实测 45s 内可复用；
// 超出后上游回 3007，届时重新求解即可。
const zaiCaptchaTTL = 45 * time.Second

// zaiCaptchaTimeout 单次求解子进程超时（求解器内部另有 ~25s 自超时）。
const zaiCaptchaTimeout = 60 * time.Second

// zaiCaptchaAttempts 求解失败重试次数。实测单次成功率约 6 成（SDK 侧偶发 stall），
// 5 次重试把整体失败率压到 1% 量级。
const zaiCaptchaAttempts = 5

// zaiCaptchaSolver 求解器的进程级单例（缓存 + 并发去重状态）。
var zaiCaptchaSolver = &zaiCaptchaState{}

type zaiCaptchaState struct {
	mu        sync.Mutex
	param     string
	fetchedAt time.Time
}

// zaiCaptchaEnabled 报告是否启用验证码求解。
// 设 ZCODE_CAPTCHA_DISABLED=1 可关闭（例如改用免验证码的 API Key 通道时）。
func zaiCaptchaEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZCODE_CAPTCHA_DISABLED"))) {
	case "1", "true", "yes", "on":
		return false
	}
	return true
}

// zaiCaptchaSolverPath 定位 captcha_node/solver.js：显式环境变量优先，
// 然后依次尝试工作目录与可执行文件所在目录（两种部署形态都覆盖）。
// 返回值一律为**绝对路径**：调用方会把它同时用作脚本参数与子进程 cwd，
// 相对路径在两者叠加下会被重复解析（captcha_node/captcha_node/solver.js）。
func zaiCaptchaSolverPath() string {
	if p := strings.TrimSpace(os.Getenv("ZCODE_CAPTCHA_SOLVER")); p != "" {
		return absPath(p)
	}
	candidates := []string{filepath.Join("captcha_node", "solver.js")}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "captcha_node", "solver.js"),
			filepath.Join(dir, "..", "captcha_node", "solver.js"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return absPath(c)
		}
	}
	return ""
}

// absPath 转绝对路径并清理（失败时原样返回，交由调用方报错）。
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func zaiNodePath() string {
	if p := strings.TrimSpace(os.Getenv("ZCODE_NODE_PATH")); p != "" {
		return p
	}
	return "node"
}

// ZaiCaptchaParam 取一个可用的验证码参数：TTL 内直接复用缓存，否则求解。
// 返回空串表示求解不可用（调用方据此决定是否继续出站——无参数时上游必回 3007）。
func ZaiCaptchaParam(ctx context.Context) string {
	if !zaiCaptchaEnabled() {
		return ""
	}
	s := zaiCaptchaSolver
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.param != "" && time.Since(s.fetchedAt) < zaiCaptchaTTL {
		return s.param
	}
	param := zaiSolveCaptcha(ctx)
	if param == "" {
		return ""
	}
	s.param = param
	s.fetchedAt = time.Now()
	return param
}

// InvalidateZaiCaptcha 丢弃缓存（上游返回 3007 时调用，强制下次重新求解）。
func InvalidateZaiCaptcha() {
	s := zaiCaptchaSolver
	s.mu.Lock()
	s.param = ""
	s.fetchedAt = time.Time{}
	s.mu.Unlock()
}

// zaiSolveCaptcha 起子进程求解，成功返回参数、失败返回空串。
// 调用方必须持有 zaiCaptchaSolver.mu（保证并发去重）。
func zaiSolveCaptcha(ctx context.Context) string {
	solverPath := zaiCaptchaSolverPath()
	if solverPath == "" {
		log.Printf("WARN: [zai_captcha] 未找到 captcha_node/solver.js：请在 captcha_node 目录执行 npm install，或用 ZCODE_CAPTCHA_SOLVER 指定路径")
		return ""
	}
	for attempt := 1; attempt <= zaiCaptchaAttempts; attempt++ {
		if param := zaiRunSolver(ctx, solverPath); param != "" {
			return param
		}
		log.Printf("WARN: [zai_captcha] 第 %d/%d 次求解未果", attempt, zaiCaptchaAttempts)
	}
	return ""
}

// zaiRunSolver 执行一次求解器调用，解析 stdout 的 VERIFY_PARAM= 行。
func zaiRunSolver(ctx context.Context, solverPath string) string {
	runCtx, cancel := context.WithTimeout(ctx, zaiCaptchaTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, zaiNodePath(), solverPath, zaiCaptchaScene, zaiCaptchaRegion, zaiCaptchaPrefix)
	cmd.Dir = filepath.Dir(solverPath) // 求解器按 cwd 解析 node_modules（happy-dom）
	// 合并 stderr：求解器把 stall 等诊断写 stderr，失败时打一行便于排障。
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "VERIFY_PARAM="); found {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	if err != nil {
		log.Printf("WARN: [zai_captcha] 求解器执行失败: %v out=%s", err, truncateForLog(string(out), 160))
	}
	return ""
}

// truncateForLog 截断子进程输出用于日志（避免刷屏）。
func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " | "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// zaiUUID4 生成一个 UUID v4 字符串（追踪头每请求全新）。
// 用 crypto/rand 取随机源；失败时退化为按时间派生（仅用于日志关联，不承担安全语义）。
func zaiUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		sum := sha1.Sum([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
		copy(b[:], sum[:16])
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const digits = "0123456789abcdef"
	h := make([]byte, 32)
	for i := 0; i < 16; i++ {
		h[i*2] = digits[b[i]>>4]
		h[i*2+1] = digits[b[i]&0x0f]
	}
	s := string(h)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// zaiDeviceMid 返回账号级设备标识（X-Device-Mid）。
// 官方客户端一号一台：这里由 UID 派生出稳定 UUID，既不跨账号复用、
// 也不依赖本机 telemetry 文件；可用 ZCODE_DEVICE_MID 显式覆盖。
func zaiDeviceMid(uid string) string {
	if v := strings.TrimSpace(os.Getenv("ZCODE_DEVICE_MID")); v != "" {
		return v
	}
	sum := sha1.Sum([]byte("zcode-device-mid:" + uid))
	sum[6] = (sum[6] & 0x0f) | 0x40 // 版本位：v4
	sum[8] = (sum[8] & 0x3f) | 0x80 // 变体位
	const digits = "0123456789abcdef"
	h := make([]byte, 32)
	for i := 0; i < 16; i++ {
		h[i*2] = digits[sum[i]>>4]
		h[i*2+1] = digits[sum[i]&0x0f]
	}
	s := string(h)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
