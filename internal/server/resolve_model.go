package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global"/"zai" 才剥离；否则视为裸名，realm=cn、
// bare=原串。大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用
// 的裸模型名。zai 裸名不默认 cn——裸名恒落 cn 池（CodeBuddy 模型名与 Z.ai 不重叠
// 时天然隔离；显式 "zai:" 前缀才是 Z.ai 路由协议）。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" && prefix != "zai" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
