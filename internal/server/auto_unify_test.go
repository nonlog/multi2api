package server

import (
    "slices"
    "strings"
    "testing"
)

func TestAutoUnifyWhitelist(t *testing.T) {
    if err := ValidateAutoUnifyModels(AutoUnifyChoices); err != nil { t.Fatal(err) }
    if err := ValidateAutoUnifyModels([]string{}); err != nil { t.Fatal(err) }
    for _, bad := range [][]string{
        {"glm-5.3-Flash-Trial"},
        {"deepseek-v4.1"},
        {"glm-5.3-flash", "glm-5.3-flash"},
        {"zcode:GLM-5.3-Flash"},
        {"glm-5.3", "anything-else"},
    } {
        if err := ValidateAutoUnifyModels(bad); err == nil {
            t.Errorf("accepted model list %v", bad)
        }
    }
}

func TestAutoUnifyExactVendorModelMatch(t *testing.T) {
    valid := []string{
        "cn:glm-5.3", "global:glm-5.3", "codearts:glm-5.3",
        "zcode:GLM-5.3-Flash", "codearts:glm-5.3-flash",
        "codearts:deepseek-v4.1-flash",
    }
    for _, id := range valid {
        canonical := strings.ToLower(strings.SplitN(id, ":", 2)[1])
        if !exactAutoSource(id, canonical) { t.Errorf("not a matching source: %s", id) }
    }
    for _, id := range []string{
        "zcode:GLM-5.3-Flash-Trial", "codearts:glm-5.3-flash-1017",
        "codearts:deepseek-v4.1-flash-1017", "codearts:glm-5.3-pro",
        "random:glm-5.3", "glm-5.3", "unknown:deepseek-v4.1-flash",
    } {
        canonical := "glm-5.3"
        if strings.Contains(id, "deepseek") { canonical = "deepseek-v4.1-flash" }
        if strings.Contains(id, "Flash") || strings.Contains(id, "flash-") { canonical = "glm-5.3-flash" }
        if exactAutoSource(id, canonical) { t.Errorf("false match: %s", id) }
    }
}

func TestAutoUnifyListingAndPriorities(t *testing.T) {
    discovered := []map[string]any{
        {"id":"cn:glm-5.3-flash", "context_length": 131072},
        {"id":"global:glm-5.3-flash", "context_length": 65536},
        {"id":"zcode:GLM-5.3-Flash", "context_length": 200000},
        {"id":"codearts:glm-5.3-flash", "context_length": 100000},
        {"id":"zcode:GLM-5.3-Flash-Trial", "context_length": 1000},
        {"id":"codearts:glm-5.3"},
    }
    enabled := []string{"glm-5.3-flash", "glm-5.3", "deepseek-v4.1-flash"}
    aliases := autoWorkBuddyAliases(map[string][]string{"deepseek-v4.1-flash":{"cn:deepseek-v4.1-flash"}}, enabled, discovered)
    if len(aliases["deepseek-v4.1-flash"]) != 1 { t.Fatal("explicit map overwritten") }
    if !slices.Equal(aliases["glm-5.3-flash"], []string{"cn:glm-5.3-flash","global:glm-5.3-flash"}) {
        t.Fatalf("wrong WorkBuddy aliases: %v", aliases)
    }
    merged := mapModelListing(discovered[:2], aliases)
    if len(merged) != 1 || merged[0]["id"] != "glm-5.3-flash" {
        t.Fatalf("CN/Global aliases not collapsed: %v", merged)
    }
    exposed := attachAutoUnifyListing(merged, discovered, enabled)
    if len(exposed) != 2 { t.Fatalf("expected one available WorkBuddy and one vendor-only auto alias, got %d", len(exposed)) }
    sources := exposed[0]["routing_sources"].([]string)
    expected := []string{"cn:glm-5.3-flash","global:glm-5.3-flash","zcode:GLM-5.3-Flash","codearts:glm-5.3-flash"}
    if !slices.Equal(sources, expected) { t.Fatalf("invalid source list (trial should be excluded): %v", sources) }
    if source := selectAutoVendor("glm-5.3-flash", discovered); source != "zcode:GLM-5.3-Flash" {
        t.Errorf("wrong vendor priority: %s", source)
    }
    if source := selectAutoVendor("glm-5.3", discovered); source != "codearts:glm-5.3" {
        t.Errorf("glm vendor missing: %s", source)
    }
    if source := selectAutoVendor("deepseek-v4.1-flash", discovered); source != "" {
        t.Errorf("should not route absent model: %s", source)
    }
}
