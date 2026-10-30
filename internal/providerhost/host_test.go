package providerhost

import "testing"

func TestRecognizeProviderModel(t *testing.T) {
	for _, model := range []string{
		"zcode:glm-5.3", "qoder:qoder-model", "qoder-cn:qoder-model",
		"qwenwork:qwen-work", "qwenwork-enterprise:qwen-work",
		"codearts:codearts-model",
	} {
		id, bare, ok := ModelProvider(model)
		if !ok || id == "" || bare == "" {
			t.Errorf("unrecognized integrated provider model: %q", model)
		}
	}
	for _, invalid := range []string{"", "glm-5.3", "cn:glm-5.3", "global:glm-5.3",
		"qoder:", ":glm", "unknown:glm", "https://example.com/secret"} {
		if _, _, ok := ModelProvider(invalid); ok {
			t.Errorf("unrecognized model %q was treated as external provider", invalid)
		}
	}
}

func TestStartRejectsMissingCredentialPathAndRuntimeFile(t *testing.T) {
	if _, err := Start("", "", ""); err == nil {
		t.Fatal("empty provider credential file was accepted")
	}
	if _, err := Start("", "/does/not/exist/worker.mjs", "/tmp/never-used.json"); err == nil {
		t.Fatal("missing plugin script was accepted")
	}
}
