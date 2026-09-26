package server

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// zaiDecodeOut 测试辅助：把转换结果解成 map。
func zaiDecodeOut(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode converted body: %v", err)
	}
	return out
}

// zaiFakeJWT 造一个 payload 为 {"user_id":uid} 的三段式 JWT（签名段无意义）。
func zaiFakeJWT(payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	return "h." + base64.RawURLEncoding.EncodeToString(raw) + ".s"
}

// TestZaiMapModelName 上游模型名大小写敏感：小写别名必须映射为官方名。
func TestZaiMapModelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"glm-5.3-flash", "GLM-5.3-Flash"},
		{"GLM-5.3-Flash", "GLM-5.3-Flash"},
		{"Glm-5.3", "GLM-5.3"},
		{"  glm-5.3  ", "GLM-5.3"},
		{"glm-turbo", "GLM-5-Turbo"},
		{"unknown-model", "unknown-model"}, // 未知名原样透传（不编造）
		{"", ""},
	}
	for _, c := range cases {
		if got := zaiMapModelName(c.in); got != c.want {
			t.Errorf("zaiMapModelName(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// TestZaiJWTUserID JWT payload → user_id（缺则回落 sub；畸形返回空串）。
func TestZaiJWTUserID(t *testing.T) {
	if got := zaiJWTUserID(zaiFakeJWT(map[string]any{"user_id": "u-123"})); got != "u-123" {
		t.Errorf("user_id = %q want u-123", got)
	}
	if got := zaiJWTUserID(zaiFakeJWT(map[string]any{"sub": "s-456"})); got != "s-456" {
		t.Errorf("sub fallback = %q want s-456", got)
	}
	if got := zaiJWTUserID(zaiFakeJWT(map[string]any{"user_id": "u-1", "sub": "s-2"})); got != "u-1" {
		t.Errorf("user_id precedence = %q want u-1", got)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.@@@.c"} {
		if got := zaiJWTUserID(bad); got != "" {
			t.Errorf("zaiJWTUserID(%q) = %q want empty", bad, got)
		}
	}
}

// TestZaiConvertRequestCore 核心约定：强制 stream、模型名映射、官方 system 身份块
// 前置（上游内容审查缺之判 3012）、metadata.user_id 注入、末条非 system 消息
// 追加 cache_control。
func TestZaiConvertRequestCore(t *testing.T) {
	jwt := zaiFakeJWT(map[string]any{"user_id": "u-777"})
	in := `{
		"model": "glm-5.3-flash",
		"stream": false,
		"max_tokens": 999999,
		"messages": [
			{"role": "system", "content": "client system prompt"},
			{"role": "user", "content": "hi"}
		]
	}`
	out := zaiDecodeOut(t, mustConvert(t, in, jwt))

	// 强制 stream:true：网关下游链路一律按 SSE 消费（客户端要非流式由 Aggregate 聚合）。
	if out["stream"] != true {
		t.Errorf("stream = %v want true（下游按 SSE 消费，必须强制）", out["stream"])
	}
	if out["model"] != "GLM-5.3-Flash" {
		t.Errorf("model = %v want GLM-5.3-Flash（上游大小写敏感）", out["model"])
	}
	if out["max_tokens"] != float64(zaiMaxTokensLimit) {
		t.Errorf("max_tokens = %v want 钳制到 %d", out["max_tokens"], zaiMaxTokensLimit)
	}

	sys, _ := out["system"].([]any)
	// 3 个官方身份块 + 1 个「powered by」块 + 客户端原 system。
	if len(sys) != len(zaiOfficialSystemBlocks)+2 {
		t.Fatalf("system blocks = %d want %d", len(sys), len(zaiOfficialSystemBlocks)+2)
	}
	for i := range zaiOfficialSystemBlocks {
		got, _ := sys[i].(map[string]any)
		want, _ := zaiOfficialSystemBlocks[i].(map[string]any)
		if got["text"] != want["text"] {
			t.Errorf("system[%d] 非官方身份块：%v", i, got["text"])
		}
	}
	powered, _ := sys[len(zaiOfficialSystemBlocks)].(map[string]any)
	if powered == nil || powered["text"] != "- You are powered by the model named GLM-5.3-Flash." {
		t.Errorf("powered-by 块缺失或模型名不符：%v", powered)
	}
	last, _ := sys[len(sys)-1].(map[string]any)
	if last["text"] != "client system prompt" {
		t.Errorf("客户端 system 未追加在官方块之后：%v", last)
	}

	meta, _ := out["metadata"].(map[string]any)
	if meta["user_id"] != "u-777" {
		t.Errorf("metadata.user_id = %v want u-777", meta)
	}

	// 末条非 system 消息末块带 cache_control（镜像官方客户端行为）。
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d want 1（system 已抽到顶层）", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("messages[0].role = %v want user", msg["role"])
	}
	blocks, _ := msg["content"].([]any)
	blk, _ := blocks[len(blocks)-1].(map[string]any)
	if cc, _ := blk["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("末块缺 cache_control：%v", blk)
	}
}

// TestZaiConvertRequestNoJWT 解不出 user_id 时跳过 metadata（不阻塞请求）。
func TestZaiConvertRequestNoJWT(t *testing.T) {
	out := zaiDecodeOut(t, mustConvert(t, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`, "bad-jwt"))
	if _, ok := out["metadata"]; ok {
		t.Errorf("畸形 JWT 不应注入 metadata：%v", out["metadata"])
	}
}

// TestZaiConvertTools 工具链路：OpenAI tools / tool_calls / role=tool 三方映射，
// 且相邻同角色消息合并（Anthropic 要求 user/assistant 严格交替）。
func TestZaiConvertTools(t *testing.T) {
	in := `{
		"model": "GLM-5.3",
		"messages": [
			{"role": "user", "content": "read a file"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a.txt\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "file body"},
			{"role": "tool", "tool_call_id": "call_2", "content": "second body"}
		],
		"tools": [{"type": "function", "function": {"name": "read", "description": "read file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}}],
		"tool_choice": "auto"
	}`
	out := zaiDecodeOut(t, mustConvert(t, in, ""))

	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "read" || tool["input_schema"] == nil {
		t.Errorf("tools[0] 形态不符 Anthropic：%v", tool)
	}
	if tc, _ := out["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Errorf("tool_choice = %v want auto", out["tool_choice"])
	}

	msgs, _ := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d want 3（两条 role=tool 应合并进同一条 user）", len(msgs))
	}
	asst, _ := msgs[1].(map[string]any)
	ablocks, _ := asst["content"].([]any)
	use, _ := ablocks[len(ablocks)-1].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "call_1" || use["name"] != "read" {
		t.Errorf("tool_use 块不符：%v", use)
	}
	if inp, _ := use["input"].(map[string]any); inp["path"] != "a.txt" {
		t.Errorf("tool_use.input 未反序列化 arguments：%v", use["input"])
	}
	usr, _ := msgs[2].(map[string]any)
	if usr["role"] != "user" {
		t.Errorf("tool_result 必须挂在 user 消息下：%v", usr["role"])
	}
	ublocks, _ := usr["content"].([]any)
	if len(ublocks) != 2 {
		t.Fatalf("合并后块数 = %d want 2", len(ublocks))
	}
	first, _ := ublocks[0].(map[string]any)
	if first["type"] != "tool_result" || first["tool_use_id"] != "call_1" || first["content"] != "file body" {
		t.Errorf("tool_result 块不符：%v", first)
	}
}

// TestZaiConvertRemoteImageRejected 远端图片 URL 显式报错（不静默丢内容）。
func TestZaiConvertRemoteImageRejected(t *testing.T) {
	in := `{"model":"GLM-5.3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	if _, err := zaiConvertRequest([]byte(in), ""); err == nil {
		t.Fatal("远端图片 URL 应报错（Z.ai 只接 base64 data URL）")
	}
}

// TestZaiConvertBase64Image stage：base64 data URL → Anthropic image 块。
func TestZaiConvertBase64Image(t *testing.T) {
	in := `{"model":"GLM-5.3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAB"}}]}]}`
	out := zaiDecodeOut(t, mustConvert(t, in, ""))
	msgs, _ := out["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	blocks, _ := msg["content"].([]any)
	img, _ := blocks[0].(map[string]any)
	src, _ := img["source"].(map[string]any)
	if img["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AAAB" {
		t.Errorf("image 块不符：%v", img)
	}
}

// mustConvert 转换失败即 Fatal。
func mustConvert(t *testing.T, in, jwt string) []byte {
	t.Helper()
	out, err := zaiConvertRequest([]byte(in), jwt)
	if err != nil {
		t.Fatalf("zaiConvertRequest: %v", err)
	}
	return out
}