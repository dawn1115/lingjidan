package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/dawn1115/lingjidan/internal/upstream"
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

// ---------------------------------------------------------------------------
// 响应方向：Anthropic SSE → OpenAI SSE
// ---------------------------------------------------------------------------

// zaiPumpRaw 跑一遍流转换，返回写出的全部 OpenAI SSE 文本。
func zaiPumpRaw(t *testing.T, sse string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := zaiPumpSSE(io.NopCloser(strings.NewReader(sse)), &buf); err != nil {
		t.Fatalf("zaiPumpSSE: %v", err)
	}
	return buf.String()
}

// zaiPumpFrames 从转换结果里取全部 data 帧载荷（不含 [DONE]）。
func zaiPumpFrames(t *testing.T, sse string) []string {
	t.Helper()
	var frames []string
	for _, line := range strings.Split(zaiPumpRaw(t, sse), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		frames = append(frames, payload)
	}
	return frames
}

// zaiStreamToolSSE 造一条「thinking 块 + 两个 tool_use 块」的上游 Anthropic 事件流。
// 关键点：tool_use 的 block index 是 1/2（被 index 0 的 thinking 块占位），
// 正是真实 GLM-5.3 出思考后再调工具时的形状。
func zaiStreamToolSSE() string {
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"glm-5.3","usage":{"input_tokens":100}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先看目录。"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_a","name":"read_file"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"/tmp/a.txt\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_b","name":"write_file"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":2}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":100,"output_tokens":20}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
}

// TestZaiStreamToolCallsShape 出站 tool_calls 必须是 OpenAI 规范形状且 index 从 0 起：
// 此前 name/arguments 平铺在 tool_call 顶层、index 直接用 Anthropic block 序号，
// 客户端（WorkBuddy/CodeBuddy 按 tool_calls[].function.name 解析）取不到工具名 →
// 工具永不执行 → 反复重问同一请求 → 被判「模型循环」中断。
func TestZaiStreamToolCallsShape(t *testing.T) {
	var toolFrames []map[string]any
	for _, frame := range zaiPumpFrames(t, zaiStreamToolSSE()) {
		var obj map[string]any
		if err := json.Unmarshal([]byte(frame), &obj); err != nil {
			t.Fatalf("帧非 JSON: %v (%s)", err, frame)
		}
		choices, _ := obj["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c, _ := choices[0].(map[string]any)
		delta, _ := c["delta"].(map[string]any)
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			toolFrames = append(toolFrames, tc)
		}
	}
	// 首片 ×2（两个 tool_use）+ 参数片 ×3。
	if len(toolFrames) != 5 {
		t.Fatalf("tool_calls 帧数 = %d want 5: %v", len(toolFrames), toolFrames)
	}
	for i, tc := range toolFrames {
		if _, flat := tc["name"]; flat {
			t.Errorf("第 %d 片 name 平铺在 tool_call 顶层（客户端取不到工具名）: %v", i, tc)
		}
		if _, flat := tc["arguments"]; flat {
			t.Errorf("第 %d 片 arguments 平铺在 tool_call 顶层（客户端拼不出参数）: %v", i, tc)
		}
		if _, ok := tc["function"].(map[string]any); !ok {
			t.Errorf("第 %d 片缺 function 对象: %v", i, tc)
		}
	}
	wantIdx := []float64{0, 0, 0, 1, 1} // thinking 块占位不影响：两个工具必须是 0/1
	for i, want := range wantIdx {
		if got, _ := toolFrames[i]["index"].(float64); got != want {
			t.Errorf("第 %d 片 index = %v want %v（不能用 Anthropic block 序号）", i, got, want)
		}
	}
}

// TestZaiStreamToolCallsAggregate 端到端：转换后的帧过 Aggregate（非流式客户端路径）
// 必须还原出带 function.name/arguments 的完整工具调用，而不是丢名字的空壳。
func TestZaiStreamToolCallsAggregate(t *testing.T) {
	raw, err := upstream.Aggregate(strings.NewReader(zaiPumpRaw(t, zaiStreamToolSSE())))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	// JSON 往返：断言客户端真正收到的线格式（Aggregate 内部用 []map[string]any）。
	buf, _ := json.Marshal(raw)
	var resp map[string]any
	if err := json.Unmarshal(buf, &resp); err != nil {
		t.Fatalf("decode aggregated response: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d want 1", len(choices))
	}
	c, _ := choices[0].(map[string]any)
	if c["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v want tool_calls", c["finish_reason"])
	}
	msg, _ := c["message"].(map[string]any)
	if msg["reasoning_content"] != "先看目录。" {
		t.Errorf("reasoning_content = %v want 先看目录。", msg["reasoning_content"])
	}
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("tool_calls = %d want 2: %v", len(calls), msg["tool_calls"])
	}
	first, _ := calls[0].(map[string]any)
	ffn, _ := first["function"].(map[string]any)
	if first["id"] != "call_a" || ffn["name"] != "read_file" {
		t.Errorf("第一个调用身份不符: %v", first)
	}
	if ffn["arguments"] != `{"path":"/tmp/a.txt"}` {
		t.Errorf("arguments = %v want {\"path\":\"/tmp/a.txt\"}（分片拼接）", ffn["arguments"])
	}
	second, _ := calls[1].(map[string]any)
	sfn, _ := second["function"].(map[string]any)
	if second["id"] != "call_b" || sfn["name"] != "write_file" || sfn["arguments"] != "{}" {
		t.Errorf("第二个调用不符: %v", second)
	}
}
