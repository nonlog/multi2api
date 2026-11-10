package main

import (
	"github.com/nonlog/multi2api/internal/pool"
	"github.com/nonlog/multi2api/internal/server"
)

// realmAwareAvailableForModelWithAliases 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
// Aliases only change the realm choice for a mapped bare name. Explicit
// cn:/global: names remain pinned for older callers and diagnostics.
func realmAwareAvailableForModelWithAliases(p *pool.Pool, aliases map[string][]string) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveMappedModel(model, aliases)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}
