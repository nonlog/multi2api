package server

import (
	"context"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// responsesNamespaceContextKey carries only the current request's declared
// namespace mappings. No process-global tool names are shared across clients.
type responsesNamespaceContextKey struct{}

// withResponsesNamespaceTools retains the canonical namespace-to-tool mapping
// when a Responses request is translated into the Chat shape. The model can
// emit either the bare tool name or namespace__tool in its native DSML text,
// while the Chat translation only declares the bare name.
func withResponsesNamespaceTools(ctx context.Context, names map[string]string) context.Context {
	if len(names) == 0 {
		return ctx
	}
	return context.WithValue(ctx, responsesNamespaceContextKey{}, names)
}

// allowlistedResponseTools extends the DSML repair allowlist only with exact
// aliases for tools DECLARED BY THIS REQUEST. Unknown names still fail closed.
// The outbound model tool schemas are unchanged; these aliases are used only
// by the server-side native-tool-text repair layer.
func allowlistedResponseTools(ctx context.Context, body []byte) map[string]bool {
	allowed := upstream.ToolNameAllowlist(body)
	if len(allowed) == 0 {
		return allowed
	}
	namespaces, ok := ctx.Value(responsesNamespaceContextKey{}).(map[string]string)
	if !ok {
		return allowed
	}
	for bare, ns := range namespaces {
		if bare == "" || ns == "" || !allowed[bare] {
			continue
		}
		allowed[ns+"__"+bare] = true
		allowed[ns+"::"+bare] = true
	}
	return allowed
}
