package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nonlog/multi2api/internal/providerhost"
)

// ValidateProviderAliases rejects ambiguous or unsafe names. An integrated
// Provider alias is a single provider:model route, not a WorkBuddy pool alias.
func ValidateProviderAliases(aliases map[string]string, wbAliases map[string][]string) error {
	for alias, target := range aliases {
		if alias == "" || len(alias) > 128 || strings.TrimSpace(alias) != alias ||
			strings.ContainsAny(alias, ":\r\n\t") {
			return fmt.Errorf("model_mapping.provider_aliases: invalid alias %q", alias)
		}
		if _, duplicate := wbAliases[alias]; duplicate {
			return fmt.Errorf("model_mapping.provider_aliases: %q already maps a WorkBuddy model", alias)
		}
		if len(target) > 256 {
			return fmt.Errorf("model_mapping.provider_aliases: invalid model target")
		}
		if _, _, ok := providerhost.ModelProvider(target); !ok || strings.TrimSpace(target) != target {
			return fmt.Errorf("model_mapping.provider_aliases: invalid built-in Provider target %q", target)
		}
	}
	return nil
}

// Publish a friendly ID only when the corresponding provider model is
// actually available in the current model catalogue. Never invent access.
func mapProviderAliases(models []map[string]any, aliases map[string]string) []map[string]any {
	if len(aliases) == 0 {
		return models
	}
	byID := map[string]map[string]any{}
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}
	for alias, target := range aliases {
		if _, taken := byID[alias]; taken {
			continue
		}
		if source, available := byID[target]; available {
			entry := make(map[string]any, len(source)+3)
			for k, v := range source {
				entry[k] = v
			}
			entry["id"] = alias
			entry["source_model"] = target
			entry["is_alias"] = true
			models = append(models, entry)
		}
	}
	return models
}

// ValidateModelAliases ensures one downstream model resolves to the same
// upstream bare ID in one or both realms. Cross-model mappings are deliberately
// rejected: the pool's per-model quotas/costs and sticky state are keyed by
// that bare model, and treating different models as the same would corrupt
// both the routing and the usage ledger.
func ValidateModelAliases(aliases map[string][]string) error {
	for alias, targets := range aliases {
		if alias == "" || strings.TrimSpace(alias) != alias || strings.Contains(alias, ":") {
			return fmt.Errorf("model_mapping alias %q must be a nonempty unprefixed model name", alias)
		}
		if len(targets) == 0 || len(targets) > 2 {
			return fmt.Errorf("model_mapping alias %q must have one or two realm targets", alias)
		}
		seen := map[string]bool{}
		bareName := ""
		for _, target := range targets {
			realm, bare := resolveModel(target)
			if (realm != "cn" && realm != "global") || target != realm+":"+bare ||
				bare == "" || strings.TrimSpace(bare) != bare {
				return fmt.Errorf("model_mapping alias %q: invalid target %q; use cn:model or global:model", alias, target)
			}
			if seen[realm] {
				return fmt.Errorf("model_mapping alias %q has duplicate realm %s", alias, realm)
			}
			seen[realm] = true
			if bareName == "" {
				bareName = bare
			} else if bareName != bare {
				return fmt.Errorf("model_mapping alias %q targets different upstream models", alias)
			}
		}
	}
	return nil
}

// ResolveMappedModel preserves explicit realm prefixes. Only the configured
// downstream alias is auto-routed across eligible realms. An empty realm is
// the pool's existing cross-realm selection mode: quotas, cooldowns, in-flight
// limits, weighting and session affinity are all retained.
func ResolveMappedModel(model string, aliases map[string][]string) (realm, bare string) {
	if targets := aliases[model]; len(targets) > 0 {
		firstRealm, name := resolveModel(targets[0])
		if len(targets) == 1 {
			return firstRealm, name
		}
		return "", name
	}
	return resolveModel(model)
}

// mapModelListing collapses only configured aliases and advertises a model
// only if at least one source realm actually returned it. Dynamic discovery
// must not invent models for a realm with no valid accounts or failed probing.
func mapModelListing(in []map[string]any, aliases map[string][]string) []map[string]any {
	if len(aliases) == 0 {
		return in
	}
	reverse := make(map[string]string)
	for alias, targets := range aliases {
		for _, target := range targets {
			reverse[target] = alias
		}
	}
	out := make([]map[string]any, 0, len(in))
	placed := make(map[string]int, len(aliases))
	for _, original := range in {
		id, _ := original["id"].(string)
		alias, mapped := reverse[id]
		if !mapped {
			out = append(out, original)
			continue
		}
		realm, _ := resolveModel(id)
		if idx, exists := placed[alias]; exists {
			merged := out[idx]
			for _, key := range []string{"context_length", "max_output_tokens"} {
				if a, aok := numberOf(merged[key]); aok {
					if b, bok := numberOf(original[key]); bok && b < a {
						merged[key] = original[key]
					}
				}
			}
			// The unprefixed name can land on EITHER realm. Advertise a
			// capability only if both variants support it.
			for _, key := range []string{"supports_images", "supports_tool_call",
				"supports_reasoning", "can_disable_thinking", "only_reasoning"} {
				if merged[key] != true || original[key] != true {
					delete(merged, key)
				}
			}
			if current, ok := merged["reasoning_supported_efforts"].([]string); ok {
				if next, ok := original["reasoning_supported_efforts"].([]string); ok {
					shared := make([]string, 0, len(current))
					for _, v := range current {
						if slices.Contains(next, v) {
							shared = append(shared, v)
						}
					}
					merged["reasoning_supported_efforts"] = shared
				} else {
					delete(merged, "reasoning_supported_efforts")
					delete(merged, "reasoning_default_effort")
				}
			}
			if realms, ok := merged["routing_realms"].([]string); ok &&
				!slices.Contains(realms, realm) {
				merged["routing_realms"] = append(realms, realm)
			}
			continue
		}
		mappedEntry := make(map[string]any, len(original)+1)
		for k, v := range original {
			mappedEntry[k] = v
		}
		mappedEntry["id"] = alias
		mappedEntry["routing_realms"] = []string{realm}
		placed[alias] = len(out)
		out = append(out, mappedEntry)
	}
	return out
}

func numberOf(raw any) (float64, bool) {
	switch n := raw.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}
