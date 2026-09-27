package server

import (
	"strings"
	"testing"

	"github.com/dawn1115/lingjidan/internal/upstream"
)

// TestResolveModel 前缀协议与「Z.ai 目录精确别名」的完整口径。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in          string
		realm, bare string
	}{
		// 显式前缀始终权威（含 realm 与同名模型的三种拼写）。
		{"cn:glm-5.3", "cn", "glm-5.3"},
		{"global:glm-5.3", "global", "glm-5.3"},
		{"global:GLM-5.3", "global", "GLM-5.3"},
		{"zai:glm-5.3", "zai", "glm-5.3"},
		{"zai:GLM-5.3", "zai", "GLM-5.3"},
		// 裸名默认落 cn。
		{"glm-5.3", "cn", "glm-5.3"},
		{"glm-5.3-flash", "cn", "glm-5.3-flash"},
		{"deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"unknown-model", "cn", "unknown-model"},
		// Z.ai 目录精确别名：大写官方名的裸名 → zai（客户端按 name 字段回传的形态）。
		{"GLM-5.3", "zai", "GLM-5.3"},
		{"GLM-5.3-Flash", "zai", "GLM-5.3-Flash"},
		// 非枚举前缀 / 前缀大小写不符 → 整体当裸名，落 cn（不触发别名）。
		{"foo:bar", "cn", "foo:bar"},
		{"CN:glm-5.3", "cn", "CN:glm-5.3"},
		{"glm-5.3:free", "cn", "glm-5.3:free"},
		{"", "cn", ""},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.realm || bare != c.bare {
			t.Errorf("resolveModel(%q) = (%q, %q) want (%q, %q)", c.in, realm, bare, c.realm, c.bare)
		}
	}
}

// TestResolveModelZaiAliasTracksCatalog 别名集合与 Z.ai 目录严格同源：目录里每个 id
// 的裸名必须落 zai，而其小写形态必须仍落 cn（CodeBuddy 自己的拼写，防未来目录改名
// 或大小写策略变化时别名跑偏）。
func TestResolveModelZaiAliasTracksCatalog(t *testing.T) {
	for _, mi := range upstream.ZaiModels() {
		if realm, bare := resolveModel(mi.ID); realm != "zai" || bare != mi.ID {
			t.Errorf("裸名 %q = (%q, %q) want (\"zai\", %q)", mi.ID, realm, bare, mi.ID)
		}
		lower := strings.ToLower(mi.ID)
		if lower == mi.ID {
			continue // 目录本就是小写：小写形态即别名本身，无需断言
		}
		if realm, _ := resolveModel(lower); realm != "cn" {
			t.Errorf("小写裸名 %q 应仍落 cn（CodeBuddy 拼写），实得 %q", lower, realm)
		}
		if realm, _ := resolveModel("zai:" + lower); realm != "zai" {
			t.Errorf("显式前缀 zai:%s 应为 zai，实得 %q", lower, realm)
		}
	}
}

// TestResolveModelBareUppercaseNotZaiWhenNotCatalog 未收录的大写名不得被别名放行：
// 别名只认目录逐字匹配，不做大小写归一（否则任何大写拼写都会被拉进 zai 域）。
func TestResolveModelBareUppercaseNotZaiWhenNotCatalog(t *testing.T) {
	for _, in := range []string{"GLM-5.4", "glm-5.4", "GLM-4.7", "DeepSeek-V4.1-Flash"} {
		if realm, bare := resolveModel(in); realm != "cn" || bare != in {
			t.Errorf("resolveModel(%q) = (%q, %q) want (\"cn\", %q)", in, realm, bare, in)
		}
	}
}
