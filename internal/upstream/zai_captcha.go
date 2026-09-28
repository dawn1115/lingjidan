// zai_captcha.go Z.ai（Start Plan 免费额度）通道所需的阿里云无痕验证参数求解。
//
// 背景：zcode.z.ai 的 coding-plan 代理端点对 JWT（免费/套餐）通道的**每个**模型请求
// 都要求 X-Aliyun-Captcha-Verify-Param。该参数由阿里云无痕 SDK 现场签发，
// **一次性**（同一参数第二次使用即被拒）；缺失/失效 → 上游 400 code 3007
// 「captcha verify failed」。
//
// 本项目不内置浏览器，改为子进程调用 Node 求解器（captcha_node/solver.js，
// 用 happy-dom 模拟浏览器环境直接跑阿里云官方 SDK）。设计对齐社区验证实现
// （dengyie/zcode2api 的 app/captcha.py）：
//   - 预解池：请求直接取一枚现成参数（亚毫秒），取走后后台异步补货；
//     池空才同步现解。**参数取走即消耗、绝不复用**——复用是并发下连环 3007 的根因。
//   - 求解单飞：同一时刻只起一个求解子进程，并发触发共享结果。
//   - 失败重试：单次求解偶发失败时自动重试（zaiCaptchaAttempts）。
//   - 挑战失效：上游回 3007 时清空整池（那批参数可能已被风控盯上）。
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

// zaiCaptchaTokenTTL 单枚验证码参数的最大可用时长（超龄即丢弃重解）。
// 参数实际 TTL 约 2 分钟，此处取 95s 留出余量（对齐社区验证实现 zcode2api 的
// CAPTCHA_TOKEN_TTL 默认值）；注意它只是"库存保鲜期"，**不代表可以复用**——
// 参数是一次性的（见 zaiCaptchaState）。
const zaiCaptchaTokenTTL = 95 * time.Second

// zaiCaptchaPoolMin / zaiCaptchaPoolMax 预解池的目标库存与上限。
// 热路径从池里取一枚（亚毫秒），取走后异步补货——避免每个请求都等一次求解。
//
// 存货量按「突发并发」定，而非按平均值：存 3 枚时，agent 一轮并发就能把池抽干，
// 之后每个请求都要**在机器满载时同步现解**——实测此时 pe VM 频繁 stall（单次
// 求解 1.5s 的健康路径退化成 5×7s 全败），请求被卡 60s 以上并拖垮在途名额，
// 连带把后续请求逼成 pool_saturated（用户可见 503）。
// 存 6 枚（上限 24）后，常见突发可整轮从存货取参，求解始终发生在空闲期。
// 正常求解仅 ~1.5s/枚，预解成本很低；TTL 95s 保证存货不会长期滞留。
const (
	zaiCaptchaPoolMin = 6
	zaiCaptchaPoolMax = 24
)

// zaiCaptchaTimeout 单次求解子进程超时（求解器内部另有 ~25s 自超时）。
const zaiCaptchaTimeout = 60 * time.Second

// zaiCaptchaAttempts 单次求解的进程内重试次数。实测单次成功率约 6 成（SDK 侧偶发 stall），
// 5 次重试把整体失败率压到 1% 量级。
const zaiCaptchaAttempts = 5

// zaiCaptchaSolver 求解器的进程级单例（预解池 + 并发去重状态）。
var zaiCaptchaSolver = &zaiCaptchaState{}

type zaiCaptchaToken struct {
	param     string
	fetchedAt time.Time
}

// zaiCaptchaState 验证码参数预解池。
//
// **关键事实：verifyParam 是一次性的**——上游对同一参数的第二次使用回 code 3007
// （社区验证实现的 get_verify_param 取出后从不放回，即此语义）。此前的实现把单枚
// 参数缓存 45s 供所有请求复用：串行时靠"撞 3007 → 失效重解 → 重试"勉强自愈，
// **并发时必然集体撞车**——N 个请求同时复用同一枚 → 全部 3007 → 一起失效重解 →
// 又拿到同一枚新参数 → 再次全部 3007，同请求重试次数耗尽 → 503。这正是
// "单账号域 + 客户端并发" 下频繁 no_healthy_account 的根因之一。
//
// 因此改为预解池：每次请求**取走**一枚（不再归还），池子由后台异步补货；
// 上游报挑战时清空整池（该批指纹可能已被盯上，继续复用只会连环 3007）。
type zaiCaptchaState struct {
	mu        sync.Mutex
	pool      []zaiCaptchaToken // FIFO；取走即消耗
	refilling bool              // 补货循环单飞
	solveMu   sync.Mutex        // 求解单飞（避免并发起多个 Node 子进程）
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

// ZaiCaptchaParam 取一枚**一次性**验证码参数：优先池内现成（亚毫秒，取走即消耗），
// 池空则同步现解一枚。返回空串表示求解不可用（调用方据此决定是否继续出站——
// 无参数时上游必回 3007）。
//
// 取出后异步触发补货，让后续请求继续命中热路径。**绝不复用**同一枚参数：
// 复用是并发下连环 3007 的根因（见 zaiCaptchaState）。
func ZaiCaptchaParam(ctx context.Context) string {
	if !zaiCaptchaEnabled() {
		return ""
	}
	s := zaiCaptchaSolver
	if p := s.take(); p != "" {
		go s.refill(context.Background())
		return p
	}
	// 池空：同步求解兜底（首启 / 补货跟不上突发并发）。补货用独立 ctx：
	// 请求 ctx 结束后仍应把库存补上。
	p := s.solveOnce(ctx)
	if p != "" {
		go s.refill(context.Background())
	}
	return p
}

// take 从池中取走一枚（一次性消耗）；池空或全部超龄返回空串。
func (s *zaiCaptchaState) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	if len(s.pool) == 0 {
		return ""
	}
	p := s.pool[0].param
	s.pool = s.pool[1:]
	return p
}

// evictLocked 丢弃超龄库存。调用方须持 s.mu。
func (s *zaiCaptchaState) evictLocked() {
	if len(s.pool) == 0 {
		return
	}
	now := time.Now()
	keep := s.pool[:0]
	for _, t := range s.pool {
		if now.Sub(t.fetchedAt) < zaiCaptchaTokenTTL {
			keep = append(keep, t)
		}
	}
	s.pool = keep
}

// solveOnce 单飞求解一枚并**直接返回**（不入池——调用方要么马上用掉，要么由
// refill 入库）。等锁期间若已有补货入库，优先取现成的，省掉一次 Node 子进程。
func (s *zaiCaptchaState) solveOnce(ctx context.Context) string {
	s.solveMu.Lock()
	defer s.solveMu.Unlock()
	if p := s.take(); p != "" {
		return p
	}
	return zaiSolveCaptcha(ctx)
}

// refill 后台补货：把库存补到 zaiCaptchaPoolMin（不超过 zaiCaptchaPoolMax）。
// 循环单飞（refilling）+ 求解单飞（solveMu）：并发触发也只会有一个补货循环、
// 一个求解子进程。
func (s *zaiCaptchaState) refill(ctx context.Context) {
	s.mu.Lock()
	if s.refilling || !zaiCaptchaEnabled() {
		s.mu.Unlock()
		return
	}
	s.refilling = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.refilling = false
		s.mu.Unlock()
	}()
	for {
		s.mu.Lock()
		s.evictLocked()
		short := len(s.pool) < zaiCaptchaPoolMin
		s.mu.Unlock()
		if !short {
			return
		}
		p := s.solveOnce(ctx)
		if p == "" {
			return // 求解不可用（Node 缺失/无外网/连续失败）：不空转，等下次触发
		}
		s.mu.Lock()
		if len(s.pool) < zaiCaptchaPoolMax {
			s.pool = append(s.pool, zaiCaptchaToken{param: p, fetchedAt: time.Now()})
		}
		s.mu.Unlock()
	}
}

// InvalidateZaiCaptcha 清空整池（上游返回 3007 时调用）：该批参数可能已被风控盯上，
// 继续复用只会连环 3007，丢掉重解才是正解。
func InvalidateZaiCaptcha() {
	s := zaiCaptchaSolver
	s.mu.Lock()
	s.pool = nil
	s.mu.Unlock()
}

// zaiSolveCaptcha 起子进程求解，成功返回参数、失败返回空串。
// 并发去重由调用方负责（solveOnce 持 solveMu 调用），本函数自身不加锁。
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
