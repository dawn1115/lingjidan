package upstream

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dawn1115/lingjidan/internal/auth"
)

// TestClassifyZai 覆盖 Z.ai（Anthropic 信封）错误分类的关键分支。
// 其中 1113 用例的响应体取自 2026-09-26 对 api.z.ai 的实测抓包（零余额账号）。
func TestClassifyZai(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{
			// 实测原文：Z.ai 用 rate_limit_error 信封承载计费耗尽，必须归硬冷却，
			// 否则零余额账号会被当软限流反复选中（回归：曾误归 ErrSoftRate）。
			name:   "1113 insufficient balance → hard credit",
			status: 429,
			body:   `{"type":"error","error":{"type":"rate_limit_error","code":"1113","message":"[1113][Insufficient balance or no resource package. Please recharge.][202609260015092cf7ad08b4aa4473]","request_id":"202609260015092cf7ad08b4aa4473"}}`,
			want:   ErrHardCredit,
		},
		{
			// 中文站文案同码同判（open.bigmodel.cn 实测）。
			name:   "1113 chinese message → hard credit",
			status: 429,
			body:   `{"type":"error","error":{"type":"rate_limit_error","code":"1113","message":"[1113][余额不足或无可用资源包,请充值。]"}}`,
			want:   ErrHardCredit,
		},
		{
			// 无 1113 的限流文案仍保持软限流语义（不可误判为硬冷却白扔号 12h）。
			name:   "plain rate limit → soft rate",
			status: 429,
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`,
			want:   ErrSoftRate,
		},
		{
			name:   "authentication error → session dead",
			status: 401,
			body:   `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`,
			want:   ErrSessionDead,
		},
		{
			name:   "permission error → account fault",
			status: 403,
			body:   `{"type":"error","error":{"type":"permission_error","message":"forbidden"}}`,
			want:   ErrAccountFault,
		},
		{
			name:   "billing error → hard credit",
			status: 402,
			body:   `{"type":"error","error":{"type":"billing_error","message":"payment required"}}`,
			want:   ErrHardCredit,
		},
		{
			name:   "prompt too long → not punished",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 204800 maximum"}}`,
			want:   ErrPromptTooLong,
		},
		{
			name:   "unknown model → model blocked",
			status: 404,
			body:   `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`,
			want:   ErrModelBlocked,
		},
		{
			name:   "overloaded → server",
			status: 529,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
			want:   ErrServer,
		},
		{
			name:   "bare 429 → soft rate",
			status: 429,
			body:   `{"error":{"message":"too many requests"}}`,
			want:   ErrSoftRate,
		},
		{
			name:   "content filter → content blocked",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"blocked by content filter"}}`,
			want:   ErrContentBlocked,
		},
		{
			name:   "success body → none",
			status: 200,
			body:   `{"id":"msg_1","type":"message","content":[]}`,
			want:   ErrNone,
		},
		{
			// 实测原文（2026-09-26 GLM-5.3 日额度用尽抓包）：上游用 HTTP 200 +
			// application/json 承载业务信封，网关需按模型级额度耗尽处置。
			name:   "1005 quota envelope on 200 → model quota",
			status: 200,
			body:   `{"code":1005,"msg":"exceed quota limit","logid":"20260926073716eb1d4ddbeaafbe02c913"}`,
			want:   ErrModelQuota,
		},
		{
			// 4xx 承载的同码同判（口径与 200 信封一致，不靠状态码）。
			name:   "1005 quota envelope on 429 → model quota",
			status: 429,
			body:   `{"code":1005,"msg":"exceed quota limit"}`,
			want:   ErrModelQuota,
		},
		{
			// 风控「unusual activity」：社区实证为 405 承载，账号级软冷却最轻。
			name:   "3012 risk control → waf block",
			status: 405,
			body:   `{"code":3012,"msg":"unusual activity, please try again later"}`,
			want:   ErrWafBlock,
		},
		{
			// 无业务码的纯文案形态（WAF 页变体）同判。
			name:   "unusual activity text without code → waf block",
			status: 405,
			body:   `{"msg":"Unusual activity detected"}`,
			want:   ErrWafBlock,
		},
		{
			// 额度耗尽不得被 rate_limit_error 信封吞掉（否则软限流短冷却反复打空池）。
			name:   "1005 beats rate_limit_error envelope",
			status: 429,
			body:   `{"type":"error","error":{"type":"rate_limit_error","code":"1005","message":"exceed quota limit"}}`,
			want:   ErrModelQuota,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyZai(c.status, c.body); got != c.want {
				t.Errorf("ClassifyZai(%d, %s) = %v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// TestZaiEnvelopeError 覆盖 HTTP 200 业务信封 → *Error 的映射（Kind + 语义状态码）。
func TestZaiEnvelopeError(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantNil    bool
		wantKind   ErrKind
		wantStatus int
	}{
		{
			name:       "1005 实测原文",
			body:       `{"code":1005,"msg":"exceed quota limit","logid":"20260926073716eb1d4ddbeaafbe02c913"}`,
			wantKind:   ErrModelQuota,
			wantStatus: 429,
		},
		{
			name:       "3012 风控",
			body:       `{"code":3012,"msg":"unusual activity"}`,
			wantKind:   ErrWafBlock,
			wantStatus: 403,
		},
		{
			name:       "3007 验证码挑战",
			body:       `{"code":3007,"msg":"captcha verify failed"}`,
			wantKind:   ErrClient,
			wantStatus: 400,
		},
		{
			name:       "未收录业务码 → client（只换号）",
			body:       `{"code":9999,"msg":"whatever"}`,
			wantKind:   ErrClient,
			wantStatus: 400,
		},
		{name: "无 code 字段 → 非信封", body: `{"id":"msg_1","type":"message"}`, wantNil: true},
		{name: "code=0 → 非信封", body: `{"code":0}`, wantNil: true},
		{name: "畸形 JSON → 非信封", body: `<html>gateway error</html>`, wantNil: true},
		{name: "空体 → 非信封", body: ``, wantNil: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := zaiEnvelopeError([]byte(c.body))
			if c.wantNil {
				if got != nil {
					t.Fatalf("zaiEnvelopeError(%q) = %+v want nil", c.body, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("zaiEnvelopeError(%q) = nil want kind %v", c.body, c.wantKind)
			}
			if got.Kind != c.wantKind || got.Status != c.wantStatus {
				t.Errorf("zaiEnvelopeError(%q) = %v/%d want %v/%d", c.body, got.Kind, got.Status, c.wantKind, c.wantStatus)
			}
		})
	}
}

// TestZaiBusinessCode 覆盖业务码提取（嵌套/字符串码/同形干扰）。
func TestZaiBusinessCode(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"code":1005,"msg":"x"}`, 1005},
		{`{"code":"1005","msg":"x"}`, 1005},
		{`{"code": 3012}`, 3012},
		{`{"data":{"code":3007}}`, 3007},
		{`{"error":{"code":1005,"message":"x"}}`, 1005},
		// logid 里含 "1005" 但无 code 字段：结构化提取不得命中（防误判整段子串）。
		{`{"msg":"x","logid":"2026092607371005eb1d4ddbeaafbe02c913"}`, 0},
		{`{"code":0}`, 0},
		{`[]`, 0},
		{``, 0},
	}
	for _, c := range cases {
		if got := zaiBusinessCode([]byte(c.body)); got != c.want {
			t.Errorf("zaiBusinessCode(%q) = %d want %d", c.body, got, c.want)
		}
	}
}

// TestZaiCaptchaChallenge 覆盖验证码挑战判定：只认窄形态，IP 级 WAF 403 不误判。
func TestZaiCaptchaChallenge(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "400 + 3007（最常见）", status: 400, body: `{"code":3007,"msg":"captcha verify failed"}`, want: true},
		{name: "200 + 3007（信封承载）", status: 200, body: `{"code":3007}`, want: true},
		{name: "403 + captcha 文案", status: 403, body: `{"msg":"captcha challenge required"}`, want: true},
		{name: "403 + verify 文案", status: 403, body: `please verify your browser`, want: true},
		{name: "403 WAF 拦截页不误判", status: 403, body: `<html><body>Forbidden</body></html>`, want: false},
		{name: "400 额度耗尽不误判", status: 400, body: `{"code":1005,"msg":"exceed quota limit"}`, want: false},
		{name: "空体不误判", status: 400, body: ``, want: false},
		{name: "2xx 正常消息体不误判", status: 200, body: `{"id":"msg_1","type":"message"}`, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := zaiCaptchaChallenge(c.status, []byte(c.body)); got != c.want {
				t.Errorf("zaiCaptchaChallenge(%d, %q) = %v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// TestZaiIsEventStream 覆盖 SSE 判定（信封分流依据）。
func TestZaiIsEventStream(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"TEXT/EVENT-STREAM", true},
		{"application/json; charset=utf-8", false},
		{"", false},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.ct != "" {
			h.Set("Content-Type", c.ct)
		}
		if got := zaiIsEventStream(h); got != c.want {
			t.Errorf("zaiIsEventStream(%q) = %v want %v", c.ct, got, c.want)
		}
	}
}

// zaiBillingBalanceBody 实测抓包的 /billing/balance 响应（2026-09-27，GLM 账号）：
// 三个额度桶，含同名模型（GLM-5.3-Flash 一次性 3 亿 + 日窗 500 万，后者已用尽）。
const zaiBillingBalanceBody = `{"code":0,"msg":"","logid":"x","data":{"server_time":1790495381,"plans":[],
"balances":[
{"bucket_id":"b1","show_name":"GLM-5.3-Flash","meter":"model_usage","unit_type":"token","total_units":300000000,"used_units":12920890,"remaining_units":287079110,"expires_at":1790557200},
{"bucket_id":"b2","show_name":"GLM-5.3","meter":"model_usage","unit_type":"token","total_units":3000000,"used_units":351023,"remaining_units":2648977,"expires_at":1790524799},
{"bucket_id":"b3","show_name":"GLM-5.3-Flash","meter":"model_usage","unit_type":"token","total_units":5000000,"used_units":5000000,"remaining_units":0,"expires_at":1790524799}
]}}`

// zaiTestClient 造一个 zai 域账号 + 指向本地假上游的 Client。
func zaiTestClient(t *testing.T, fn rtFunc) (*Client, *auth.Auth) {
	t.Helper()
	c := testClient(fn)
	c.ZaiBillingBase = "https://zcode.example/api/v1/zcode-plan"
	a := &auth.Auth{UID: "tok-abc", AccessToken: "jwt-token"}
	if _, err := auth.BackfillRealmFor(a, "zai"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}
	return c, a
}

// TestZaiUserResourceAggregation 智谱额度查询：走 zcode.z.ai 的 billing/balance，
// 各额度桶求和；并核对出站形态（GET + Bearer + X-Device-Mid，与官方客户端一致）。
func TestZaiUserResourceAggregation(t *testing.T) {
	c, a := zaiTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, fmt.Errorf("want GET, got %s", r.Method)
		}
		if r.URL.Host != "zcode.example" || r.URL.Path != "/api/v1/zcode-plan/billing/balance" {
			return nil, fmt.Errorf("wrong endpoint: %s%s", r.URL.Host, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer jwt-token" {
			return nil, fmt.Errorf("bad auth header: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Device-Mid") == "" {
			return nil, fmt.Errorf("missing X-Device-Mid（上游会回 code 3001）")
		}
		if r.Header.Get("x-request-id") == "" {
			return nil, fmt.Errorf("missing x-request-id")
		}
		return jsonResp(200, zaiBillingBalanceBody), nil
	})
	remain, total, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("zai user resource: %v", err)
	}
	// remain = 287079110 + 2648977 + 0；total = 300000000 + 3000000 + 5000000。
	if remain != 289728087 {
		t.Errorf("remain=%d want 289728087", remain)
	}
	if total != 308000000 {
		t.Errorf("total=%d want 308000000", total)
	}
}

// TestUserResourceRoutesZaiRealm zai 域不得落到 CodeBuddy 的 billing base——
// 回归：billingBase 只区分 cn/global，zai 账号曾被当成 CN 打 CodeBuddy 计费端点
// 并被其 apisix 判 401（面板「刷新余额」必失败）。
func TestUserResourceRoutesZaiRealm(t *testing.T) {
	var hitCN bool
	c, a := zaiTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "billing.example" {
			hitCN = true
			return jsonResp(401, `<html><head><title>401 Authorization Required</title></head></html>`), nil
		}
		if r.URL.Host != "zcode.example" {
			return nil, fmt.Errorf("unexpected host: %s", r.URL.Host)
		}
		return jsonResp(200, zaiBillingBalanceBody), nil
	})
	if _, _, err := c.UserResource(a); err != nil {
		t.Fatalf("zai 账号应走智谱 billing，实得错误: %v", err)
	}
	if hitCN {
		t.Fatal("zai 账号被路由到了 CodeBuddy 计费端点（billingBase 未按 zai 分流）")
	}
	// 对照：cn 账号仍必须走 CodeBuddy（分流不得反向影响既有域）。
	cnAcct := &auth.Auth{UID: "u1", AccessToken: "at"}
	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "billing.example" {
			return nil, fmt.Errorf("cn 账号应走 CodeBuddy billing，实得 %s", r.URL.Host)
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":60,"CycleCapacityUsed":40}]}}}}`), nil
	})
	remain, total, err := c2.UserResource(cnAcct)
	if err != nil {
		t.Fatalf("cn user resource: %v", err)
	}
	if remain != 60 || total != 100 {
		t.Errorf("cn remain/total = %d/%d want 60/100", remain, total)
	}
}

// TestZaiUserResourceExpiring 快过期子集：expires_at 落在 soon 窗口内的桶计入 expiring。
func TestZaiUserResourceExpiring(t *testing.T) {
	now := time.Now()
	soon := now.Add(2 * time.Hour).Unix()
	later := now.Add(400 * time.Hour).Unix()
	body := fmt.Sprintf(`{"code":0,"data":{"balances":[
		{"total_units":100,"remaining_units":40,"expires_at":%d},
		{"total_units":200,"remaining_units":50,"expires_at":%d},
		{"total_units":300,"remaining_units":0,"expires_at":%d}
	]}}`, soon, later, soon)
	c, a := zaiTestClient(t, func(*http.Request) (*http.Response, error) {
		return jsonResp(200, body), nil
	})
	remain, total, expiring, err := c.UserResourceDetailed(a, 168*time.Hour)
	if err != nil {
		t.Fatalf("zai user resource detailed: %v", err)
	}
	if remain != 90 || total != 600 {
		t.Errorf("remain/total = %d/%d want 90/600", remain, total)
	}
	// 仅第一个桶（40）在 2h 窗口内；第三个桶虽然也在此刻到期但剩余为 0，不计入。
	if expiring != 40 {
		t.Errorf("expiring=%d want 40", expiring)
	}
	// soon<=0 时禁用分桶（与 CodeBuddy 侧同口径）。
	if _, _, exp, _ := c.UserResourceDetailed(a, 0); exp != 0 {
		t.Errorf("soon<=0 时 expiring=%d want 0", exp)
	}
}

// TestZaiUserResourceErrors 上游错误/空桶必须显式报错（不伪装成 0 额度成功）。
func TestZaiUserResourceErrors(t *testing.T) {
	t.Run("业务码非 0", func(t *testing.T) {
		c, a := zaiTestClient(t, func(*http.Request) (*http.Response, error) {
			return jsonResp(200, `{"code":3001,"msg":"parameter error: device_mid required"}`), nil
		})
		if _, _, err := c.UserResource(a); err == nil {
			t.Fatal("code!=0 应报错")
		}
	})
	t.Run("401 鉴权失败", func(t *testing.T) {
		c, a := zaiTestClient(t, func(*http.Request) (*http.Response, error) {
			return jsonResp(401, `<html><head><title>401 Authorization Required</title></head></html>`), nil
		})
		if _, _, err := c.UserResource(a); err == nil {
			t.Fatal("401 应报错（不返回 0 额度冒充成功）")
		}
	})
	t.Run("空额度桶", func(t *testing.T) {
		c, a := zaiTestClient(t, func(*http.Request) (*http.Response, error) {
			return jsonResp(200, `{"code":0,"data":{"balances":[]}}`), nil
		})
		if _, _, err := c.UserResource(a); err == nil {
			t.Fatal("空桶应报错")
		}
	})
}

// ---------------------------------------------------------------------------
// 验证码预解池（一次性消耗）
// ---------------------------------------------------------------------------

// withCaptchaPool 用给定参数预置全局池（不跑真实求解器），测试结束还原。
func withCaptchaPool(t *testing.T, params ...string) *zaiCaptchaState {
	t.Helper()
	old := zaiCaptchaSolver
	s := &zaiCaptchaState{}
	now := time.Now()
	for _, p := range params {
		s.pool = append(s.pool, zaiCaptchaToken{param: p, fetchedAt: now})
	}
	zaiCaptchaSolver = s
	t.Cleanup(func() { zaiCaptchaSolver = old })
	return s
}

// TestZaiCaptchaTakeConsumes 参数**一次性**：取走即消耗，同一枚绝不二次发放。
// 回归：旧实现把单枚参数缓存 45s 全员复用，并发下集体 3007。
func TestZaiCaptchaTakeConsumes(t *testing.T) {
	s := withCaptchaPool(t, "p1")
	if got := s.take(); got != "p1" {
		t.Fatalf("take=%q want p1", got)
	}
	if got := s.take(); got != "" {
		t.Errorf("已取走的参数不得再次发放，实得 %q", got)
	}
}

// TestZaiCaptchaTakeConcurrentDistinct 并发取参必须各自拿到**不同**的参数：
// 复用同一枚是并发下连环 3007 的根因（回归守卫）。
func TestZaiCaptchaTakeConcurrentDistinct(t *testing.T) {
	const n = 20
	params := make([]string, 0, n)
	for i := 0; i < n; i++ {
		params = append(params, fmt.Sprintf("p%d", i))
	}
	s := withCaptchaPool(t, params...)

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := s.take()
			mu.Lock()
			seen[p]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Errorf("并发取到 %d 个不同参数 want %d（不得重复发放）", len(seen), n)
	}
	for p, c := range seen {
		if c != 1 {
			t.Errorf("参数 %q 被发放 %d 次（必须恰好一次）", p, c)
		}
	}
}

// TestZaiCaptchaEvictExpired 超龄库存必须丢弃（TTL 是库存保鲜期，不是复用窗口）。
func TestZaiCaptchaEvictExpired(t *testing.T) {
	s := withCaptchaPool(t)
	s.pool = []zaiCaptchaToken{
		{param: "fresh", fetchedAt: time.Now()},
		{param: "stale", fetchedAt: time.Now().Add(-zaiCaptchaTokenTTL - time.Minute)},
	}
	if got := s.take(); got != "fresh" {
		t.Fatalf("take=%q want fresh（超龄的应被丢弃）", got)
	}
	if got := s.take(); got != "" {
		t.Errorf("超龄参数不得发放，实得 %q", got)
	}
}

// TestInvalidateZaiCaptchaClearsPool 上游报挑战时清空整池（那批参数可能已被盯上）。
func TestInvalidateZaiCaptchaClearsPool(t *testing.T) {
	s := withCaptchaPool(t, "p1", "p2")
	InvalidateZaiCaptcha()
	if got := s.take(); got != "" {
		t.Errorf("失效后池应为空，实得 %q", got)
	}
	s.mu.Lock()
	n := len(s.pool)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("池长度=%d want 0", n)
	}
}
