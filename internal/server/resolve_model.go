package server

import (
	"strings"

	"github.com/dawn1115/lingjidan/internal/upstream"
)

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global"/"zai" 才剥离；否则视为裸名，realm=cn、
// bare=原串。大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用
// 的裸模型名。
//
// 裸名默认落 cn 池，唯一例外是「Z.ai 目录精确别名」：裸名逐字（大小写敏感）等于
// Z.ai 静态目录中的模型 id 时路由到 zai 域。为什么需要该例外：Z.ai 官方模型名是
// 大写（GLM-5.3），cn 池 CodeBuddy 的同名模型是小写（glm-5.3），两种拼写不同、
// 可精确区分；而客户端若直接拿 Z.ai 官方名当模型名（ZCode 桌面端
// provider_config.json 的 modelId 即 "GLM-5.3"；或按模型清单的 name 字段回传——
// modelList 对 zai 条目透出的 name 正是 "GLM-5.3"），按旧口径「裸名恒落 cn」会打到
// CodeBuddy 并被上游立即拒（实测：0 token、224ms 报错），但该大写拼写在 cn 侧本就
// 不存在（cn 目录只有小写 glm-5.3）——故别名不改变任何既有可用行为，只是把本就无解
// 的请求接到唯一能服务它的域。
//
// 边界：显式前缀（cn:/global:/zai:）始终权威，不受该例外影响；小写裸名（glm-5.3）
// 仍落 cn（CodeBuddy 自己的拼写）；未收录名（含 "foo:bar" 这类非枚举前缀）仍按裸名
// 落 cn。别名不查池状态（确定性路由）：池内无 zai 账号时该请求返回常规
// no_healthy_account，与「打到 cn 被上游拒」同为失败，不产生新语义。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		if isZaiCatalogName(model) {
			return "zai", model
		}
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" && prefix != "zai" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// isZaiCatalogName 报告 name 是否逐字等于 Z.ai 静态目录（upstream.ZaiModels）中的
// 某个模型 id（大小写敏感）。目录仅 2 条静态记录，逐请求线性比较的开销可忽略；
// 也刻意不用包级 map 缓存——目录是 static 常量表，缓存只会带来初始化顺序与一致性
// 的额外讨论成本。
func isZaiCatalogName(name string) bool {
	if name == "" {
		return false
	}
	for _, mi := range upstream.ZaiModels() {
		if mi.ID == name {
			return true
		}
	}
	return false
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
