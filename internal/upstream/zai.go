// zai.go 封装 Z.ai（GLM）上游调用：Anthropic 兼容端点 + x-api-key 鉴权。
//
// Z.ai 账号（realm=zai）与 CodeBuddy 账号共用同一个 auth.Auth 结构与 pool 状态机，
// 差异只在出站端点/头/协议：
//   - 端点：https://api.z.ai/api/anthropic/v1/messages（Anthropic Messages 协议）
//   - 鉴权：x-api-key: <token>（长期 JWT，无 refresh 概念）
//   - 协议：入站 OpenAI /v1/chat/completions 请求 → 出站前转 Anthropic 请求；
//     上游 Anthropic SSE → 转回 OpenAI SSE/聚合响应。
//
// 转换层拆在 internal/server/convert_zai.go（handler 按realm 分派），
// 本文件只做传输与错误分类（与 CodeBuddy 的 client.go 同构）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dawn1115/lingjidan/internal/auth"
	"github.com/dawn1115/lingjidan/internal/logfmt"
)

// ZaiPlanChatBase 免费额度/套餐（Start Plan / Coding Plan）通道 base。
// 这是 ZCode 客户端专属代理端点，鉴权用 zcodejwttoken，且**每个模型请求都需
// 携带 X-Aliyun-Captcha-Verify-Param**（见 zai_captcha.go）。
const ZaiPlanChatBase = "https://zcode.z.ai/api/v1/zcode-plan/anthropic"

// ZaiApiKeyChatBase API Key 通道 base（api.z.ai）：按量计费、免验证码。
// 保留作为未来 API Key 回退通道使用。
const ZaiApiKeyChatBase = "https://api.z.ai/api/anthropic"

// ZaiChatBase 默认出站 base：走免费额度 Plan 通道。
const ZaiChatBase = ZaiPlanChatBase

// ZaiPlanBillingBase Z.ai Plan 通道的套餐/额度查询 base（与 chat 同域、不同路径）。
// 端点（均 GET，需 Bearer JWT + X-Device-Mid；缺 Device-Mid 上游回 code 3001）：
//   - {base}/billing/current  套餐与授权（plans[].entitlements）
//   - {base}/billing/balance  各模型额度桶（total_units/used_units/remaining_units/expires_at）
//   - {base}/usage            用量
const ZaiPlanBillingBase = "https://zcode.z.ai/api/v1/zcode-plan"

// ZaiAnthropicVersion Anthropic 版本头（Z.ai 网关接受该值；缺省 conservative）。
const ZaiAnthropicVersion = "2023-06-01"

// ── 官方客户端身份头常量（指纹层，逐字段对齐 ZCode 桌面端）────────────────────
//
// 为什么必须伪装：上游 WAF 对请求做指纹一致性校验——服务端部署在 Linux 时若按
// 真实 platform 上报（linux/云内核）会与官方桌面端形状矛盾，抬高风控关注度。
// 社区实证可行形态为 macOS arm64 桌面端，故固定该组值。
//
// 出处：社区验证实现 dengyie/zcode2api 的 app/constants.py + app/identity.py。
const (
	zaiAppVersion       = "3.11.2"
	zaiClientPlatform   = "darwin-arm64"
	zaiOsCategory       = "macos"
	zaiOsVersion        = "25.5.0" // darwin 25.x 对应 macOS 15
	zaiReleaseChannel   = "stable"
	zaiClientTitle      = "Z Code@electron"
	zaiClientLanguage   = "zh-CN"
	zaiClientTimezone   = "Asia/Shanghai"
	zaiAgentHeader      = "glm"
	zaiHTTPReferer      = "https://zcode.z.ai/"
	zaiCaptchaParamHdr  = "X-Aliyun-Captcha-Verify-Param"
	zaiCaptchaRegionHdr = "X-Aliyun-Captcha-Verify-Region"
	zaiCaptchaRegionVal = "cn"

	// zaiMaxTokensLimit 上游 max_tokens 合法上限（实测超限报 400 code 1210）。
	zaiMaxTokensLimit = 131072

	// zaiCaptchaRetries 验证码挑战的同请求内重试次数（0 = 不重试）。
	zaiCaptchaRetries = 1
)

// ClassifyZai 按 Anthropic 错误信封分类（映射到与 CodeBuddy 相同的 ErrKind 枚举，
// pool 状态机/applyErrorPolicy 零改动复用）。
//
// Anthropic 错误形态：{"type":"error","error":{"type":"rate_limit_error","message":"..."}}
// 错误 type 映射：
//   - rate_limit_error        → ErrSoftRate（429 语义，短冷却自愈）
//     例外：信封携带 code=1113（余额不足/无可用资源包）→ ErrHardCredit（不可自愈）
//   - authentication_error    → ErrSessionDead（token 失效，禁用待人工换号）
//   - permission_error        → ErrAccountFault（账号级授权故障，冷却轮换）
//   - billing_error / insufficient_balance → ErrHardCredit（余额耗尽，硬冷却）
//   - 业务码 1005「exceed quota limit」    → ErrModelQuota（**模型级**日额度耗尽）
//   - 业务码 3012 / "unusual activity"    → ErrWafBlock（上游风控，账号软冷却）
//   - invalid_request_error + prompt too long 文案 → ErrPromptTooLong（不罚号不轮转）
//   - invalid_request_error   → ErrBadParams（请求体问题，不罚号仍轮转）
//   - api_error / overloaded_error / 5xx → ErrServer
//   - 429（无信封）           → ErrSoftRate
//   - 401（无信封）           → ErrSessionDead
//   - 其他 4xx                → ErrClient
func ClassifyZai(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// prompt too long 最先判：invalid_request_error 信封里带 "prompt is too long"
	// 时是请求级错误（不罚号不轮转），比通用映射更具体。
	if status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge {
		for _, m := range promptTooLongMarkers {
			if strings.Contains(body, m) || (m != strings.ToLower(m) && strings.Contains(lower, strings.ToLower(m))) {
				return ErrPromptTooLong
			}
		}
	}
	// 1113 =「Insufficient balance or no resource package」的结构化业务码。Z.ai 用
	// rate_limit_error 信封承载这类**计费耗尽**（实测 429 + error.type=rate_limit_error
	// + code=1113），语义上不可自愈，必须先于下方 rate_limit_error→ErrSoftRate 映射。
	// 若按软限流处理，零余额账号会留在池内被反复选中，白耗每个请求的一次轮转机会；
	// 硬冷却（至次日 04:00）才是正确语义。对齐 CodeBuddy 侧 429+14018 的修复哲学
	// （client.go issue #175）：只认结构化业务码，不靠文案猜测。
	if hasBusinessCode(body, "1113") {
		return ErrHardCredit
	}
	// 1005「exceed quota limit」：模型日额度池耗尽（实测经 200 业务信封承载，
	// 也可能随 4xx 状态码出现）。必须先于 rate_limit_error→ErrSoftRate 映射：
	// 额度耗尽不可自愈，按软限流处理会让请求反复打空配额模型。
	if hasBusinessCode(body, "1005") {
		return ErrModelQuota
	}
	// 3012「unusual activity」上游风控（实测 HTTP 405 承载，社区亦见 200 信封）。
	// 按账号级软冷却 + IP 级 fail-fast 处理：继续换号重打只会加剧风控。
	if hasBusinessCode(body, "3012") || strings.Contains(lower, "unusual activity") {
		return ErrWafBlock
	}
	// Anthropic 信封 error.type 精确判定（优先于状态码——上游偶发 400 + rate_limit_error）。
	if errType := zaiErrorType(body); errType != "" {
		switch errType {
		case "rate_limit_error":
			return ErrSoftRate
		case "authentication_error":
			return ErrSessionDead
		case "permission_error", "forbidden_error":
			return ErrAccountFault
		case "billing_error", "insufficient_balance_error":
			return ErrHardCredit
		case "invalid_request_error":
			// content filter / sensitive content → 内容拦截（不罚号）
			for _, m := range zaiContentBlockedMarkers {
				if strings.Contains(lower, m) {
					return ErrContentBlocked
				}
			}
			return ErrBadParams
		case "api_error", "overloaded_error", "internal_server_error":
			return ErrServer
		case "not_found_error":
			return ErrModelBlocked
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusUnauthorized:
		return ErrSessionDead
	case status == http.StatusForbidden:
		// 无信封 403：IP 级 WAF 形态（与 CodeBuddy 同哲学）
		if !hasBusinessEnvelope(body) {
			return ErrWafBlock
		}
		return ErrAccountFault
	case status == http.StatusPaymentRequired:
		return ErrHardCredit
	case status >= 500:
		return ErrServer
	case status >= 400:
		for _, m := range zaiContentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		return ErrClient
	}
	return ErrNone
}

// zaiErrorType 提取 Anthropic 错误信封的 error.type 字段（无信封返回空串）。
func zaiErrorType(body string) string {
	if body == "" || !strings.Contains(body, `"type"`) {
		return ""
	}
	var root struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return ""
	}
	return strings.TrimSpace(root.Error.Type)
}

// zaiContentBlockedMarkers Z.ai 内容拦截关键词（小写比较）。
var zaiContentBlockedMarkers = []string{
	"content filter",
	"sensitive content",
	"content policy",
	"内容安全",
}

// zaiChatHTTP 聊天 SSE 专用请求：无总时长上限，由调用方 ctx + monitorBody 看门狗接管。
func (c *Client) zaiChatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// ZaiChatStreamContext Z.ai（realm=zai）chat 出站：POST {base}/v1/messages。
//
// 走的是**免费额度/套餐 Plan 通道**（zcode.z.ai 的 coding-plan 代理端点）：
//   - 鉴权：Authorization: Bearer <zcodejwttoken>（实测同一 token 用 x-api-key 被判 401）
//   - 身份头：伪装官方 ZCode macOS 桌面端（见 applyZaiIdentityHeaders）
//   - 追踪头：start-plan 通道**只发** x-request-id / x-zcode-session-type /
//     x-zcode-trace-id；误发 x-query-id / x-session-id 会被上游判 3012
//   - 验证码：每个请求都要带 X-Aliyun-Captcha-Verify-Param（+ Region），
//     缺失即 400 / code 3007；参数由 zai_captcha.go 求解并缓存
//
// body 已由 server 转换层转成 Anthropic Messages 请求体（含官方 system 身份块）。
// 返回值语义与 ChatStreamContext 一致：成功返回原始 SSE 流（Anthropic 事件流），
// 错误路径返回已分类 *Error（Kind 信封 + Retry-After 解析）。
//
// 验证码挑战在本函数内**同账号重试**（zaiCaptchaRetries）：verifyParam 实际 TTL
// 仅数十秒，跨 TTL 复用会被拒；验证码问题不是账号问题，故不轮转不罚号，失效缓存
// 重解后重试一次（对齐社区实现「换码重试一次」语义）。
func (c *Client) ZaiChatStreamContext(ctx context.Context, a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 0; ; attempt++ {
		rc, status, respBody, err = c.zaiChatOnce(ctx, a, body)
		if attempt >= zaiCaptchaRetries || !zaiCaptchaChallenge(status, respBody) {
			return rc, status, respBody, err
		}
		InvalidateZaiCaptcha()
		log.Printf("WARN: [upstream] zai_chat acct=%s: 验证码挑战（status=%d），已失效缓存重解并重试（第 %d/%d 次）",
			logfmt.Label(a.UID, a.Nickname), status, attempt+1, zaiCaptchaRetries)
	}
}

// zaiChatOnce 单次出站（不含验证码重试）。语义与旧 ZaiChatStreamContext 一致。
func (c *Client) zaiChatOnce(ctx context.Context, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	endpoint := ZaiChatBase + "/v1/messages"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("anthropic-version", ZaiAnthropicVersion)
	c.applyZaiIdentityHeaders(req, a)
	// 验证码：取不到时仍继续出站（让上游给出权威错误码），但先记一行便于排障。
	if param := ZaiCaptchaParam(ctx); param != "" {
		req.Header.Set(zaiCaptchaParamHdr, param)
		req.Header.Set(zaiCaptchaRegionHdr, zaiCaptchaRegionVal)
	} else {
		log.Printf("WARN: [upstream] zai_chat acct=%s: 未取到验证码参数，上游大概率回 3007", logfmt.Label(a.UID, a.Nickname))
	}
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)

	resp, err := c.zaiChatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] zai_chat acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
		roundTripCloseIdle(c.zaiChatHTTP().Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			log.Printf("ERR: [upstream] zai_chat acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		kind := ClassifyZai(resp.StatusCode, string(raw))
		// 3007 = 验证码挑战失败：参数多半已过期（TTL 数十秒），失效缓存让下次重新求解。
		if hasBusinessCode(string(raw), "3007") {
			InvalidateZaiCaptcha()
			log.Printf("WARN: [upstream] zai_chat acct=%s: 上游要求重新验证码（3007），已失效本地缓存", logfmt.Label(a.UID, a.Nickname))
		}
		log.Printf("WARN: [upstream] zai_chat acct=%s: upstream %d %s body=%s",
			logfmt.Label(a.UID, a.Nickname), resp.StatusCode, kind, truncate(string(raw), 200))
		if kind == ErrNone {
			return nil, resp.StatusCode, raw, nil
		}
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			ue.RetryAfter = d
		}
		return nil, resp.StatusCode, raw, ue
	}
	// 业务信封：上游对「请求合法但被服务端拒绝」的场景用 **HTTP 200** 承载
	// {"code":N,"msg":...}（实测 GLM-5.3 额度用尽：200 + application/json +
	// code=1005）。SSE 流的 Content-Type 恒为 text/event-stream，故 2xx 里
	// 非 event-stream 的一律按信封读取——否则这层 JSON 会被下游当作
	// 「无有效 data 帧」上报，真实业务码在日志里丢失。
	if !zaiIsEventStream(resp.Header) {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			log.Printf("ERR: [upstream] zai_chat acct=%s: read envelope: %v", logfmt.Label(a.UID, a.Nickname), rerr)
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		if ue := zaiEnvelopeError(raw); ue != nil {
			if ue.Kind == ErrClient && hasBusinessCode(string(raw), "3007") {
				InvalidateZaiCaptcha()
			}
			log.Printf("WARN: [upstream] zai_chat acct=%s: upstream 200+业务信封 %s body=%s",
				logfmt.Label(a.UID, a.Nickname), ue.Kind, truncate(string(raw), 200))
			return nil, ue.Status, raw, ue
		}
		// 无业务码的 2xx 非流式响应：上游未按 stream:true 返回 SSE（协议异常）。
		// 下游只认 SSE 帧，此处显式报错，好过下游「无有效 data 事件」的模糊归因。
		ue := &Error{
			Kind:   ErrServer,
			Status: http.StatusBadGateway,
			Msg: fmt.Sprintf("upstream returned non-SSE response (content-type=%q): %s",
				resp.Header.Get("Content-Type"), truncate(string(raw), 200)),
		}
		log.Printf("WARN: [upstream] zai_chat acct=%s: %s", logfmt.Label(a.UID, a.Nickname), ue.Msg)
		return nil, ue.Status, raw, ue
	}
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// zaiIsEventStream 报告响应是否为 SSE 流（Content-Type 含 text/event-stream）。
func zaiIsEventStream(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// zaiEnvelopeError 把 HTTP 200 业务信封 {"code":N,"msg":...} 分类为 *Error；
// 非信封（无 code 或 code=0）返回 nil。
//
// Status 是**语义映射**（上游本身给的是 200），供日志/流水与客户端可见状态使用：
// 1005 额度耗尽 → 429（与 OpenAI insufficient_quota 同口径）、3012 风控 → 403、
// 其余 → 400。Kind 才是策略依据（applyErrorPolicy 只看 Kind）。
func zaiEnvelopeError(raw []byte) *Error {
	code := zaiBusinessCode(raw)
	if code == 0 {
		return nil
	}
	status := http.StatusBadRequest
	switch code {
	case 1005:
		status = http.StatusTooManyRequests
	case 3012:
		status = http.StatusForbidden
	}
	return &Error{Kind: zaiEnvelopeKind(code), Status: status, Msg: truncate(string(raw), 200)}
}

// zaiEnvelopeKind 业务信封 code → ErrKind。只收录 chat 通道实测/社区确证的码，
// 未收录码归 ErrClient（只换号，不重罚、不熔断）。
func zaiEnvelopeKind(code int) ErrKind {
	switch code {
	case 1005:
		// 「exceed quota limit」：该模型**日额度池**耗尽（实测 GLM-5.3 日 300 万
		// token 用尽后每请求必回此码），非同账号其它模型（如 GLM-5.3-Flash）
		// 的额度。故走模型级避让而非账号级硬冷却。
		return ErrModelQuota
	case 3012:
		// 「unusual activity」上游风控（官方定性为临时限制）：账号级软冷却 +
		// IP 级 fail-fast 计数（继续换号重打只会加剧风控）。
		return ErrWafBlock
	case 3007:
		// 验证码挑战：正常路径已由 zaiCaptchaChallenge 重试消化，走到这里说明
		// 重解后仍被拒。验证码问题不是账号问题，不重罚。
		return ErrClient
	default:
		return ErrClient
	}
}

// zaiBusinessCode 提取业务信封的 code 字段（顶层/嵌套皆可，缺省 0）。
func zaiBusinessCode(body []byte) int {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"code"`)) {
		return 0
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return 0
	}
	var walk func(any) int
	walk = func(v any) int {
		switch node := v.(type) {
		case map[string]any:
			if c, ok := node["code"]; ok {
				switch n := c.(type) {
				case float64:
					return int(n)
				case string:
					var parsed int
					if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &parsed); err == nil {
						return parsed
					}
				}
			}
			for _, child := range node {
				if got := walk(child); got != 0 {
					return got
				}
			}
		case []any:
			for _, child := range node {
				if got := walk(child); got != 0 {
					return got
				}
			}
		}
		return 0
	}
	return walk(root)
}

// zaiCaptchaChallenge 判定一次出站是否撞上验证码挑战。三种承载形态：
//   - 400 + body {"code":3007}（最常见）；
//   - 200 + 同信封（上游偶用 200 承载业务码）；
//   - 403 + captcha/verify 文案（WAF 挑战页，无业务码）。
//
// 只认这些窄形态：403 的 WAF 拦截页（无 captcha 文案）不在此列，由 ClassifyZai
// 归 ErrWafBlock，避免把 IP 级拦截误当验证码反复重解。
func zaiCaptchaChallenge(status int, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if hasBusinessCode(string(body), "3007") {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	low := strings.ToLower(string(body))
	return strings.Contains(low, "captcha") || strings.Contains(low, "verify")
}

// applyZaiIdentityHeaders 写官方客户端身份头与追踪头（指纹层）。
//
// 身份头逐字段对齐 ZCode 桌面端（macOS arm64 形态）：任缺项或形状不符都会抬高
// WAF 关注度。追踪头**只发三个**——start-plan 通道若多发 x-query-id /
// x-session-id，上游会直接判 3012「unusual activity」（社区实证 + 本项目复现）。
func (c *Client) applyZaiIdentityHeaders(req *http.Request, a *auth.Auth) {
	c.applyZaiIdentityBase(req, a)
	req.Header.Set("X-ZCode-Agent", zaiAgentHeader)
	// 追踪头：每请求全新 UUID；start-plan 严禁 x-query-id / x-session-id。
	req.Header.Set("x-request-id", zaiUUID4())
	req.Header.Set("x-zcode-session-type", "main")
	req.Header.Set("x-zcode-trace-id", zaiUUID4())
}

// applyZaiIdentityBase 写 messages 与 billing 共用的客户端身份头（chat 的额外追踪头
// 与 billing 的鉴权头各自在包装函数里追加，见上/下两处）。
func (c *Client) applyZaiIdentityBase(req *http.Request, a *auth.Auth) {
	ua := c.UserAgent
	if ua == "" {
		ua = "ZCode/" + zaiAppVersion
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("HTTP-Referer", zaiHTTPReferer)
	req.Header.Set("X-ZCode-App-Version", zaiAppVersion)
	req.Header.Set("X-Title", zaiClientTitle)
	req.Header.Set("X-Platform", zaiClientPlatform)
	req.Header.Set("X-Release-Channel", zaiReleaseChannel)
	req.Header.Set("X-Client-Language", zaiClientLanguage)
	req.Header.Set("X-Client-Timezone", zaiClientTimezone)
	req.Header.Set("X-Os-Category", zaiOsCategory)
	req.Header.Set("X-Os-Version", zaiOsVersion)
	req.Header.Set("X-Device-Mid", zaiDeviceMid(a.UID))
}

// applyZaiBillingHeaders billing（套餐/额度）出站头：身份头 + Bearer + x-request-id。
//
// 刻意**不发** x-zcode-session-type / x-zcode-trace-id —— 官方客户端 billing 请求不带
// 这两个 chat 专属追踪头（社区实证形态 zai_billing_headers），多带只会扩大指纹面。
func (c *Client) applyZaiBillingHeaders(req *http.Request, a *auth.Auth) {
	c.applyZaiIdentityBase(req, a)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("x-request-id", zaiUUID4())
}

// zaiBillingBase 生效的 Z.ai billing base（测试可经 Client.ZaiBillingBase 覆盖）。
func (c *Client) zaiBillingBase() string {
	if c.ZaiBillingBase != "" {
		return c.ZaiBillingBase
	}
	return ZaiPlanBillingBase
}

// zaiUserResource 查询 Z.ai 账号额度（GET {base}/billing/balance）：
// 把 data.balances[] 各模型额度桶按 total_units / remaining_units 求和。
//
// 为什么必须独立于 CodeBuddy 的 billing：两者是完全不同的计费后端（智谱是
// zcode.z.ai 的按模型 token 桶，CodeBuddy 是 workbuddy 的积分套餐表）。此前
// billingBase 只区分 cn/global，zai 账号被当成 CN、拿 JWT 去打 CodeBuddy 计费端点，
// 被其 apisix 网关判 401 —— 面板「刷新余额」对智谱账号必失败（issue：智谱余额未适配）。
//
// expiring 语义与 CodeBuddy 侧一致（快过期子集，供选号优先消耗）：soon > 0 时把
// expires_at ≤ now+soon 的桶剩余额度计入；soon ≤ 0 恒 0。remain 负值钳 0。
func (c *Client) zaiUserResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	req, err := http.NewRequest(http.MethodGet, c.zaiBillingBase()+"/billing/balance", nil)
	if err != nil {
		return 0, 0, 0, err
	}
	c.applyZaiBillingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, err
	}
	var resp struct {
		Balances []struct {
			TotalUnits     int64 `json:"total_units"`
			RemainingUnits int64 `json:"remaining_units"`
			// ExpiresAt 额度桶到期时刻（epoch 秒；0 = 无到期）。
			ExpiresAt int64 `json:"expires_at"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, fmt.Errorf("zai balance parse: %w", err)
	}
	if len(resp.Balances) == 0 {
		// 空桶 = 无可判定的额度（不编造 0 额度冒充"查询成功"）。
		return 0, 0, 0, fmt.Errorf("zai balance: upstream returned no balance buckets")
	}
	now := time.Now()
	for _, b := range resp.Balances {
		total += b.TotalUnits
		if b.RemainingUnits <= 0 {
			continue
		}
		remain += b.RemainingUnits
		if soon > 0 && b.ExpiresAt > 0 && !time.Unix(b.ExpiresAt, 0).After(now.Add(soon)) {
			expiring += b.RemainingUnits
		}
	}
	if remain < 0 {
		remain = 0
	}
	return remain, total, expiring, nil
}

// zaiModelCatalog Z.ai Plan 通道（免费额度/套餐）可用的模型静态目录。
//
// 只列**套餐实际授权**的模型：Start Plan / Coding Plan 现为 GLM-5.3 与
// GLM-5.3-Flash。上游对未授权模型回 3006「model not allowed」，列多了会让
// 客户端选到必失败的型号（社区实证：5.2/5-Turbo 不在当前套餐内）。
// API Key 通道（api.z.ai，按量计费）另有更宽的目录，接该通道时再放宽。
var zaiModelCatalog = []ModelInfo{
	{ID: "GLM-5.3", Name: "GLM-5.3", ContextWindow: 1048576, MaxTokens: zaiMaxTokensLimit, SupportsToolCall: true, SupportsReasoning: true},
	{ID: "GLM-5.3-Flash", Name: "GLM-5.3-Flash", ContextWindow: 1048576, MaxTokens: zaiMaxTokensLimit, SupportsToolCall: true, SupportsReasoning: true},
}

// ZaiModels 返回 Z.ai 静态模型目录（handler /v1/models 的 zai 段）。
// 仅当池内有可用 zai 账号时才被调用（无号 → 空名单，不编造）。
func ZaiModels() []ModelInfo {
	out := make([]ModelInfo, len(zaiModelCatalog))
	copy(out, zaiModelCatalog)
	return out
}

// zaiKeepaliveAt Z.ai 账号的保活语义：x-api-key 长期有效，无需定时刷新。
// scheduler 按 realm 跳过（见 scheduler.go 的 realm 过滤），此函数仅为文档自洽。
func zaiKeepaliveAt() time.Time { return time.Time{} }
