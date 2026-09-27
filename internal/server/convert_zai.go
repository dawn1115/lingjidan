package server

// Z.ai（realm=zai）协议转换层：OpenAI Chat Completions ↔ Anthropic Messages。
//
// Z.ai 账号走 zcode.z.ai 的 coding-plan 代理端点（Plan 通道，见
// internal/upstream/zai.go：Bearer <zcodejwttoken> + 身份头仿真 + 验证码头），
// 网关对外仍暴露 OpenAI 协议，双向转换全部收在本文件：
//   - zaiConvertRequest：OpenAI 请求体 → Anthropic Messages 请求体（出站前一次，
//     与账号无关，轮转复用）；
//   - zaiWrapStream：Anthropic SSE 流 → OpenAI SSE 帧流（io.Reader 层就地转换），
//     包装后下游 chatStatsReader / StreamHint / Aggregate 全链路零改动复用。
//
// 流式转换语义（与下游两消费方精确对齐）：
//   - message_stop 时显式发 data: [DONE]：StreamHint 遇 [DONE] 终止读取后统一
//     自写恰好一个 [DONE]（双写安全）；Aggregate 依此置 sawDone，避免
//     dropTruncatedToolCalls 误删完整 tool_calls。上游中途断流（EOF 无
//     message_stop）则不发 [DONE]，让 Aggregate 按截断语义丢弃未闭合 tool_calls。
//   - error 事件 → data: {"error":{...}} 帧：StreamHint 对带 error 键的帧
//     writeRaw 原样透传（绕过 normalizeFrame 白名单），错误信息无损到达客户端。
//   - usage 帧：message_start 带 prompt_tokens、message_delta 带 completion_tokens
//     /total_tokens（chatStatsReader 取末帧，后帧覆盖 → 最终值正确）。
//   - tool_use → OpenAI tool_calls 帧：必须是规范形状（id/type + function.name /
//     function.arguments 嵌套），index 用 0 起的工具序号而非 Anthropic block 序号。
//     平铺的 name/arguments（或稀疏 index）会让客户端取不到工具名（WorkBuddy/CodeBuddy
//     按 tc.function?.name 解析）→ 工具永不执行 → 客户端用同一上下文反复重问 →
//     模型重复输出同一段思考 → 被客户端判「模型循环」并中断（issue：zai 工具链路）。

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 请求转换：OpenAI Chat → Anthropic Messages
// ---------------------------------------------------------------------------

// zaiDefaultMaxTokens Anthropic 端 max_tokens 必填；OpenAI 客户端缺省时的兜底值。
const zaiDefaultMaxTokens = 8192

// zaiMaxTokensLimit 上游 max_tokens 合法上限（实测超限报 400 code 1210）。
// 客户端（如 auto-compact 续传）可能带更大值，统一钳制。
const zaiMaxTokensLimit = 131072

// zaiModelNameMap 客户端小写别名 → 上游官方模型名。
// 上游模型名**大小写敏感**：小写 glm-5.3 会被拒，必须映射为 GLM-5.3。
// 出处：社区验证实现 dengyie/zcode2api 的 app/constants.py MODEL_NAME_MAP。
var zaiModelNameMap = map[string]string{
	"glm-5.3-flash": "GLM-5.3-Flash",
	"glm-5.3":       "GLM-5.3",
	"glm-5.2":       "GLM-5.2",
	"glm-5-turbo":   "GLM-5-Turbo",
	"glm-turbo":     "GLM-5-Turbo",
	"glm-5.1":       "GLM-5.1",
	"glm-5":         "GLM-5",
	"glm-4.7":       "GLM-4.7",
	"glm-4.6":       "GLM-4.6",
	"glm-4.5":       "GLM-4.5",
	"glm-4.5-air":   "GLM-4.5-Air",
}

// zaiOfficialSystemBlocks 官方 ZCode system 身份块（自官方客户端 bundle 提取）。
//
// 为什么要前置：上游网关对请求体做内容审查，**缺少这些身份块会被判
// 3012「unusual activity」**（社区实证，本项目亦复现）。注入后客户端原有
// system 追加在其后，语义不丢。
var zaiOfficialSystemBlocks = []any{
	map[string]any{
		"type":          "text",
		"text":          "You are ZCode, an interactive coding agent",
		"cache_control": map[string]any{"type": "ephemeral"},
	},
	map[string]any{
		"type": "text",
		"text": "\nYou are an interactive ZCode agent that helps users with software engineering tasks.\n\n" +
			"IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts. " +
			"Refuse requests for destructive techniques, DoS attacks, mass targeting, supply chain compromise, or detection evasion for malicious purposes. " +
			"Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: " +
			"pentesting engagements, CTF competitions, security research, or defensive use cases.\n\n" +
			"# Harness\n" +
			"- Text you output outside of tool use is displayed to the user as Github-flavored markdown in a terminal.\n" +
			"- Tools run behind a user-selected permission mode; a denied call means the user declined it — adjust, don't retry verbatim.\n" +
			"- The system may send updates, reminders, or modifications to rules via mid-conversation system turns. These are system-controlled, unlike function results. Hooks may intercept tool calls; treat hook output as user feedback.\n" +
			"- Prefer the dedicated file/search tools over shell commands when one fits. Independent tool calls can run in parallel in one response.\n" +
			"- Reference code as `file_path:line_number` — it's clickable.",
		"cache_control": map[string]any{"type": "ephemeral"},
	},
	map[string]any{
		"type": "text",
		"text": "# Environment\nYou have been invoked in the following environment:\n" +
			"- Primary working directory: unknown\n- Is a git repository: no\n- Platform: unknown\n- Shell: unknown\n- OS Version: unknown",
		"cache_control": map[string]any{"type": "ephemeral"},
	},
}

// zaiMapModelName 把客户端模型名映射为上游官方名（大小写敏感，未知名原样透传）。
func zaiMapModelName(name string) string {
	if mapped, ok := zaiModelNameMap[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mapped
	}
	return name
}

// zaiJWTUserID 从 JWT payload 解 user_id（sub / user_id 字段）。
// 官方客户端用它在请求体注入 metadata.user_id；解不出则返回空串（跳过注入）。
func zaiJWTUserID(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		UserID string `json:"user_id"`
		Sub    string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if claims.UserID != "" {
		return claims.UserID
	}
	return claims.Sub
}

// zaiApplyCacheControl 给最后一条非 system 消息的最后一个 content block 追加
// cache_control（镜像官方客户端 finalizeLatestNonSystemCacheControl）。
// Anthropic 对低于缓存门槛的请求静默忽略该标记，故无条件追加是安全的。
func zaiApplyCacheControl(msgs []any) {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if r, _ := msg["role"].(string); r == "system" {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok || len(blocks) == 0 {
			return
		}
		if last, ok := blocks[len(blocks)-1].(map[string]any); ok {
			if _, exists := last["cache_control"]; !exists {
				last["cache_control"] = map[string]any{"type": "ephemeral"}
			}
		}
		return
	}
}

// zaiConvertRequest 把 OpenAI Chat Completions 请求体转换为 Anthropic Messages
// 请求体。入参必须是合法 OpenAI JSON（handler 侧已 peek 过），失败仅可能是客户端
// 传了畸形结构（图片 URL 非法等），返回的错误直接透传给客户端 400。
//
// jwt 为当前尝试账号的令牌：用于解出 user_id 注入 metadata（官方客户端行为）。
func zaiConvertRequest(body []byte, jwt string) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("zai: decode openai request: %w", err)
	}

	out := map[string]any{}
	model := ""
	if m, _ := in["model"].(string); m != "" {
		model = zaiMapModelName(m)
		out["model"] = model
	}
	// 强制 stream:true：与 CodeBuddy 侧 payload.go 同约定——网关下游链路
	// （chatStatsReader / StreamHint / Aggregate）一律按 SSE 消费，上游非流式
	// 单 JSON 会被判「无有效 data 帧」。客户端要非流式时由 Aggregate 自行聚合。
	out["stream"] = true
	// max_tokens：Anthropic 必填，OpenAI 缺省 → 兜底（防上游 400 invalid_request）；
	// 超出上游上限则钳制（实测超限报 400 code 1210）。
	mt := zaiDefaultMaxTokens
	if v, ok := in["max_tokens"].(float64); ok && v > 0 {
		mt = int(v)
	}
	if mt > zaiMaxTokensLimit {
		mt = zaiMaxTokensLimit
	}
	out["max_tokens"] = mt
	if v, ok := in["temperature"].(float64); ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"].(float64); ok {
		out["top_p"] = v
	}
	switch v := in["stop"].(type) {
	case string:
		if v != "" {
			out["stop_sequences"] = []string{v}
		}
	case []any:
		var seqs []string
		for _, s := range v {
			if str, ok := s.(string); ok && str != "" {
				seqs = append(seqs, str)
			}
		}
		if len(seqs) > 0 {
			out["stop_sequences"] = seqs
		}
	}

	// system：OpenAI system/developer 消息（任意位置）抽为顶层 system 文本块序列。
	// prompt.Rewrite 等 CodeBuddy 指纹逻辑对 zai 已在 handler 侧跳过，此处拿到
	// 的即客户端原始 system。
	var sysBlocks []any
	msgs := make([]map[string]any, 0, 16)
	appendMsg := func(role string, blocks []any) {
		// Anthropic 要求 user/assistant 严格交替：相邻同角色块合并进上一条。
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
			prev, _ := msgs[n-1]["content"].([]any)
			msgs[n-1]["content"] = append(prev, blocks...)
			return
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
	}

	rawMsgs, _ := in["messages"].([]any)
	for _, rm := range rawMsgs {
		m, ok := rm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			for _, b := range zaiTextBlocks(m["content"]) {
				sysBlocks = append(sysBlocks, b)
			}
			continue
		}

		var blocks []any
		switch role {
		case "assistant":
			if s, ok := m["content"].(string); ok && s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			} else {
				blocks = append(blocks, zaiContentBlocks(m["content"])...)
			}
			// tool_calls → tool_use 块（arguments 反序列化回对象；坏 JSON 归并空对象）。
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tcm["function"].(map[string]any)
					name, _ := fn["name"].(string)
					if name == "" {
						continue
					}
					id, _ := tcm["id"].(string)
					var input any = map[string]any{}
					if args, _ := fn["arguments"].(string); args != "" {
						var parsed any
						if json.Unmarshal([]byte(args), &parsed) == nil {
							input = parsed
						}
					}
					blocks = append(blocks, map[string]any{
						"type": "tool_use", "id": id, "name": name, "input": input,
					})
				}
			}
		case "tool":
			// role tool → user + tool_result 块（tool_use_id 必带，空值上游报错由客户端侧修正）。
			tcID, _ := m["tool_call_id"].(string)
			blocks = append(blocks, map[string]any{
				"type": "tool_result", "tool_use_id": tcID, "content": zaiToolResultContent(m["content"]),
			})
		default: // user 及未知角色一律按 user 处理
			if s, ok := m["content"].(string); ok && s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			} else {
				var err error
				blocks, err = zaiUserBlocks(m["content"])
				if err != nil {
					return nil, err
				}
			}
		}
		if len(blocks) > 0 {
			// Anthropic 无 tool 角色：tool_result 块必须挂在 user 消息下
			//（OpenAI 连续多条 role=tool 会自然合并进同一条 user 消息）。
			dst := role
			if dst != "assistant" {
				dst = "user"
			}
			appendMsg(dst, blocks)
		}
	}
	if len(msgs) > 0 {
		list := make([]any, len(msgs))
		for i, m := range msgs {
			list[i] = m
		}
		// cache_control：最后一条非 system 消息末块（镜像官方客户端行为）。
		zaiApplyCacheControl(list)
		out["messages"] = list
	}
	// 官方 system 身份块必须前置（上游内容审查，缺失判 3012），客户端原有 system 其后。
	official := append([]any{}, zaiOfficialSystemBlocks...)
	if model != "" {
		official = append(official, map[string]any{
			"type":          "text",
			"text":          "- You are powered by the model named " + model + ".",
			"cache_control": map[string]any{"type": "ephemeral"},
		})
	}
	out["system"] = append(official, sysBlocks...)
	// metadata.user_id：官方客户端从 JWT 解出后注入（解不出则跳过，不阻塞请求）。
	if uid := zaiJWTUserID(jwt); uid != "" {
		out["metadata"] = map[string]any{"user_id": uid}
	}

	// tools / tool_choice（choice=none 等效于不带工具）。
	choiceNone := false
	switch tc := in["tool_choice"].(type) {
	case string:
		switch tc {
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "none":
			choiceNone = true
		default: // auto 及未知串 → auto
			out["tool_choice"] = map[string]any{"type": "auto"}
		}
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if n, _ := fn["name"].(string); n != "" {
				out["tool_choice"] = map[string]any{"type": "tool", "name": n}
			}
		}
	}
	if !choiceNone {
		if arr, ok := in["tools"].([]any); ok {
			var tools []any
			for _, t := range arr {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				schema, ok := fn["parameters"]
				if !ok || schema == nil {
					schema = map[string]any{"type": "object"}
				}
				tools = append(tools, map[string]any{
					"name":         name,
					"description":  fn["description"],
					"input_schema": schema,
				})
			}
			if len(tools) > 0 {
				out["tools"] = tools
			}
		}
	}

	return json.Marshal(out)
}

// zaiTextBlocks 把字符串或 content 数组归并为纯文本块列表（system 抽取用）。
func zaiTextBlocks(content any) []any {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": s}}
	}
	var out []any
	arr, ok := content.([]any)
	if !ok {
		return nil
	}
	for _, p := range arr {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := pm["text"].(string); t != "" {
			out = append(out, map[string]any{"type": "text", "text": t})
		}
	}
	return out
}

// zaiContentBlocks assistant 数组 content 的文本块提取（tool 调用块走 tool_calls 字段）。
func zaiContentBlocks(content any) []any {
	arr, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, p := range arr {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := pm["text"].(string); t != "" {
			out = append(out, map[string]any{"type": "text", "text": t})
		}
	}
	return out
}

// zaiUserBlocks user 数组 content：text 直映；image_url 仅支持 base64 data URL
// （Z.ai Anthropic 兼容层接受 base64 source）；远端 http URL / 音频等不支持，
// 返回错误由调用方 400 透传（不静默丢内容——丢图换回答是静默降级，比报错更糟）。
func zaiUserBlocks(content any) ([]any, error) {
	arr, ok := content.([]any)
	if !ok {
		return nil, nil
	}
	var out []any
	for _, p := range arr {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := pm["type"].(string); t {
		case "text":
			if s, _ := pm["text"].(string); s != "" {
				out = append(out, map[string]any{"type": "text", "text": s})
			}
		case "image_url":
			iu, _ := pm["image_url"].(map[string]any)
			u, _ := iu["url"].(string)
			if u == "" {
				return nil, fmt.Errorf("zai: image_url.url is empty")
			}
			if !strings.HasPrefix(u, "data:") {
				return nil, fmt.Errorf("zai: remote image URLs are not supported, send base64 data URLs")
			}
			rest := strings.TrimPrefix(u, "data:")
			semi := strings.Index(rest, ";base64,")
			if semi < 0 {
				return nil, fmt.Errorf("zai: image data URL must be base64 encoded")
			}
			media := rest[:semi]
			if media == "" {
				media = "image/png"
			}
			out = append(out, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": media, "data": rest[semi+len(";base64,"):],
				},
			})
		default:
			// input_audio / file 等不支持类型：跳过（不报错——客户端消息里常混有
			// 可忽略的占位块，报错会放大不兼容面；文本/图片已覆盖主流用法）。
		}
	}
	return out, nil
}

// zaiToolResultContent role=tool 的 content 归并为字符串（Anthropic tool_result
// content 接受 string；数组取 text 拼接）。
func zaiToolResultContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range arr {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := pm["text"].(string); t != "" {
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 流式转换：Anthropic SSE → OpenAI SSE（io.Reader 包装）
// ---------------------------------------------------------------------------

// zaiWrapStream 把 Z.ai 返回的 Anthropic SSE 流包装成 OpenAI SSE 帧流。
// 返回的 ReadCloser 供下游 chatStatsReader/StreamHint/Aggregate 直接消费；
// Close 级联关闭上游 rc（经 pipe 错误传导终止 pump goroutine）。
func zaiWrapStream(rc io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := zaiPumpSSE(rc, pw)
		pw.CloseWithError(err) // err == nil → 正常 EOF
		rc.Close()
	}()
	return pr
}

// zaiStreamState 单条流的转换状态（goroutine 私有，无需加锁）。
type zaiStreamState struct {
	w           io.Writer
	done        bool
	id          string
	model       string
	inputTokens int
	blocks      map[int]string // Anthropic block index → content_block type
	// toolIndex Anthropic block index → 出站 tool_calls.index（0 起递增的工具序号）。
	// 不能直接用 block index：Anthropic 的 block 序号含 text/thinking 块，出现思考时
	// 首个 tool_use 可能落在 1/2…，而 OpenAI 客户端（WorkBuddy/CodeBuddy）按
	// tool_calls.index 归位片段，稀疏起点会让片段找不到所属调用（工具不执行）。
	toolIndex map[int]int
	toolSeq   int
}

// zaiPumpSSE 逐事件读 Anthropic SSE 并写出 OpenAI 帧，直到 EOF 或写失败。
func zaiPumpSSE(rc io.ReadCloser, w io.Writer) error {
	br := bufio.NewReaderSize(rc, 64*1024)
	st := &zaiStreamState{w: w, blocks: map[int]string{}, toolIndex: map[int]int{}}
	event := ""
	for {
		line, rerr := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload != "" && payload != "[DONE]" {
				if err := st.dispatch(event, payload); err != nil {
					return err
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil // 无 message_stop 的 EOF = 上游截断，交由下游按截断语义处理
			}
			return rerr
		}
	}
}

// dispatch 按事件类型分派（event: 行缺失时回退到 payload 内 type 字段——部分
// 中间层会剥 event 行）。
func (st *zaiStreamState) dispatch(event, payload string) error {
	if st.done {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return nil // 非 JSON 心跳/垃圾行：忽略
	}
	etype, _ := obj["type"].(string)
	if etype == "" {
		etype = event
	}
	switch etype {
	case "message_start":
		return st.onMessageStart(obj)
	case "content_block_start":
		return st.onBlockStart(obj)
	case "content_block_delta":
		return st.onBlockDelta(obj)
	case "message_delta":
		return st.onMessageDelta(obj)
	case "message_stop":
		st.done = true
		return st.emitRaw("data: [DONE]\n\n")
	case "error":
		return st.emitError(obj)
	default: // ping 及未知事件：忽略
		return nil
	}
}

// onMessageStart 发首帧（id/model/role + prompt usage），缓存流级元数据。
func (st *zaiStreamState) onMessageStart(obj map[string]any) error {
	msg, _ := obj["message"].(map[string]any)
	st.id, _ = msg["id"].(string)
	st.model, _ = msg["model"].(string)
	if u, ok := msg["usage"].(map[string]any); ok {
		if v, ok := u["input_tokens"].(float64); ok {
			st.inputTokens = int(v)
		}
	}
	return st.emitJSON(map[string]any{
		"id": st.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": st.model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": ""},
		}},
		"usage": map[string]any{
			"prompt_tokens": st.inputTokens, "completion_tokens": 0, "total_tokens": st.inputTokens,
		},
	})
}

// onBlockStart 记录 block 类型；tool_use 发首片（id/name 一次），text/thinking
// 带起始内容时直接发内容帧。
func (st *zaiStreamState) onBlockStart(obj map[string]any) error {
	idx, _ := obj["index"].(float64)
	i := int(idx)
	cb, _ := obj["content_block"].(map[string]any)
	bt, _ := cb["type"].(string)
	st.blocks[i] = bt
	switch bt {
	case "text":
		if t, _ := cb["text"].(string); t != "" {
			return st.emitDelta(map[string]any{"content": t})
		}
	case "thinking":
		if t, _ := cb["thinking"].(string); t != "" {
			return st.emitDelta(map[string]any{"reasoning_content": t})
		}
	case "tool_use":
		id, _ := cb["id"].(string)
		name, _ := cb["name"].(string)
		// 出站必须是 OpenAI 规范形状：name/arguments 嵌在 function 对象内（客户端
		// 按 tool_calls[].function.name 取工具名，平铺的 name 会被丢弃 → 工具不执行）。
		ti := st.toolSeq
		st.toolSeq++
		st.toolIndex[i] = ti
		return st.emitDelta(map[string]any{"tool_calls": []any{map[string]any{
			"index": ti, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": ""},
		}}})
	}
	return nil
}

// onBlockDelta 增量转换：text_delta→content、thinking_delta→reasoning_content、
// input_json_delta→tool_calls.function.arguments 分片（index 用该 tool_use 块的
// 出站工具序号，Aggregate 按 index 归并）。
func (st *zaiStreamState) onBlockDelta(obj map[string]any) error {
	idx, _ := obj["index"].(float64)
	d, _ := obj["delta"].(map[string]any)
	switch dt, _ := d["type"].(string); dt {
	case "text_delta":
		if t, _ := d["text"].(string); t != "" {
			return st.emitDelta(map[string]any{"content": t})
		}
	case "thinking_delta":
		if t, _ := d["thinking"].(string); t != "" {
			return st.emitDelta(map[string]any{"reasoning_content": t})
		}
	case "input_json_delta":
		if p, _ := d["partial_json"].(string); p != "" {
			// index 用该 tool_use 块的出站序号（与首片一致）；未见首片时（上游丢帧）
			// 回落 block 序号兜底——好过丢弃分片让参数残缺。
			ti, ok := st.toolIndex[int(idx)]
			if !ok {
				ti = int(idx)
			}
			return st.emitDelta(map[string]any{"tool_calls": []any{map[string]any{
				"index":    ti,
				"function": map[string]any{"arguments": p},
			}}})
		}
	}
	return nil
}

// onMessageDelta 发 finish_reason（stop_reason 映射）+ 末帧 usage。
func (st *zaiStreamState) onMessageDelta(obj map[string]any) error {
	d, _ := obj["delta"].(map[string]any)
	sr, _ := d["stop_reason"].(string)
	fr := ""
	switch sr {
	case "max_tokens":
		fr = "length"
	case "tool_use":
		fr = "tool_calls"
	case "end_turn", "stop_sequence", "":
		if sr != "" {
			fr = "stop"
		}
	default:
		fr = "stop"
	}
	choice := map[string]any{"index": 0, "delta": map[string]any{}}
	if fr != "" {
		choice["finish_reason"] = fr
	}
	frame := map[string]any{"choices": []any{choice}}
	if u, ok := obj["usage"].(map[string]any); ok {
		out, _ := u["output_tokens"].(float64)
		if v, ok := u["input_tokens"].(float64); ok && int(v) > st.inputTokens {
			st.inputTokens = int(v)
		}
		frame["usage"] = map[string]any{
			"prompt_tokens": st.inputTokens, "completion_tokens": int(out),
			"total_tokens": st.inputTokens + int(out),
		}
	}
	return st.emitJSON(frame)
}

// emitError Anthropic error 事件 → OpenAI error 帧（StreamHint writeRaw 原样透传）。
func (st *zaiStreamState) emitError(obj map[string]any) error {
	e, _ := obj["error"].(map[string]any)
	msg, _ := e["message"].(string)
	t, _ := e["type"].(string)
	if t == "" {
		t = "upstream_error"
	}
	return st.emitJSON(map[string]any{"error": map[string]any{
		"message": msg, "type": t, "code": t,
	}})
}

// emitDelta 发一个标准 choices[0].delta 帧。
func (st *zaiStreamState) emitDelta(delta map[string]any) error {
	return st.emitJSON(map[string]any{
		"id": st.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model":   st.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
}

func (st *zaiStreamState) emitJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	_, err = io.WriteString(st.w, "data: "+string(b)+"\n\n")
	return err
}

func (st *zaiStreamState) emitRaw(s string) error {
	_, err := io.WriteString(st.w, s)
	return err
}
