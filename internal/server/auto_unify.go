package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/nonlog/multi2api/internal/providerhost"
)

// The current automatic routing feature deliberately supports only these
// three exact model identities. It never treats a fast/flash variant as its
// base model, nor a vendor-internal short ID as a known upstream family.
var AutoUnifyChoices = []string{"deepseek-v4.1-flash", "glm-5.3-flash", "glm-5.3"}

func ValidateAutoUnifyModels(models []string) error {
	seen := map[string]bool{}
	for _, model := range models {
		if !slices.Contains(AutoUnifyChoices, model) || seen[model] {
			return fmt.Errorf("model_mapping.auto_unify_models: unknown or duplicate model %q", model)
		}
		seen[model] = true
	}
	return nil
}

// exactAutoSource matches the model's canonical vendor-specific name but
// preserves the actual case and prefix for outgoing API requests.
func exactAutoSource(id, model string) bool {
	p := strings.SplitN(id, ":", 2)
	if len(p) != 2 || p[1] == "" {
		return false
	}
	switch p[0] {
	case "cn", "global", "zcode", "codearts", "qoder", "qoder-cn", "qwenwork", "qwenwork-enterprise":
		return strings.EqualFold(p[1], model)
	}
	return false
}

func autoWorkBuddyAliases(explicit map[string][]string, enabled []string, discovered []map[string]any) map[string][]string {
	if len(enabled) == 0 {
		return explicit
	}
	aliases := make(map[string][]string, len(explicit)+len(enabled))
	for k, v := range explicit {
		aliases[k] = v
	}
	for _, model := range enabled {
		if len(aliases[model]) > 0 {
			continue
		}
		var targets []string
		for _, realm := range []string{"cn", "global"} {
			id := realm + ":" + model
			for _, listed := range discovered {
				if listed["id"] == id {
					targets = append(targets, id)
					break
				}
			}
		}
		if len(targets) > 0 {
			aliases[model] = targets
		}
	}
	return aliases
}

// Describe each automatic mapping with every *actually discovered* source.
// Preserve vendor-qualified IDs for clients that intentionally pin a
// vendor. WorkBuddy realm aliases follow the established collapsible naming
// policy. A canonical alias is emitted only if at least one source exists.
func attachAutoUnifyListing(list, discovered []map[string]any, enabled []string) []map[string]any {
	for _, model := range enabled {
		sources := make([]string, 0, 5)
		seen := map[string]bool{}
		for _, item := range discovered {
			id, _ := item["id"].(string)
			if exactAutoSource(id, model) && !seen[id] {
				seen[id] = true
				sources = append(sources, id)
			}
		}
		if len(sources) == 0 {
			continue
		}
		var aliased map[string]any
		for _, item := range list {
			if item["id"] == model {
				aliased = item
				break
			}
		}
		if aliased == nil {
			for _, item := range discovered {
				if item["id"] == sources[0] {
					aliased = make(map[string]any, len(item)+3)
					for k, v := range item {
						aliased[k] = v
					}
					aliased["id"] = model
					list = append(list, aliased)
					break
				}
			}
		}
		if aliased != nil {
			aliased["auto_unified"] = true
			aliased["routing_sources"] = sources
		}
	}
	return list
}

type autoProviderHost interface {
	Models(context.Context) []map[string]any
	ProxyChat(http.ResponseWriter, *http.Request, []byte, string, string, func())
}

func autoVendors(model string, discovered []map[string]any) []string {
	var routes []string
	seen := map[string]bool{}
	for _, item := range discovered {
		id, _ := item["id"].(string)
		provider, _, ok := providerhost.ModelProvider(id)
		if ok && !seen[provider] && exactAutoSource(id, model) {
			routes = append(routes, id)
			seen[provider] = true
		}
	}
	slices.Sort(routes)
	return routes
}

// Cross-provider routing leaves account selection to each source. The empty
// route represents the existing WorkBuddy pool.
func (h *Handler) routeAutoChat(w http.ResponseWriter, r *http.Request, body []byte, model string, providers autoProviderHost, workBuddyReady bool, workBuddy func(func())) {
	routes := autoVendors(model, providers.Models(r.Context()))
	if len(routes) == 0 {
		workBuddy(nil)
		return
	}
	if workBuddyReady {
		routes = append([]string{""}, routes...)
	}
	cursor, _ := h.autoRouteCursor.LoadOrStore(model, &atomic.Uint64{})
	start := (cursor.(*atomic.Uint64).Add(1) - 1) % uint64(len(routes))
	var attempt func(int)
	attempt = func(i int) {
		route := routes[(start+uint64(i))%uint64(len(routes))]
		var fallback func()
		if i+1 < len(routes) {
			fallback = func() {
				if route != "" {
					log.Printf("WARN: [server] auto route model=%s source=%s unavailable, trying another provider", model, route)
				}
				attempt(i + 1)
			}
		}
		if route == "" {
			workBuddy(fallback)
			return
		}
		provider, bare, _ := providerhost.ModelProvider(route)
		providers.ProxyChat(w, r, body, provider, bare, fallback)
	}
	attempt(0)
}
