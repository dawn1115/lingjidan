package upstream

import (
	"net/http"
	"testing"
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