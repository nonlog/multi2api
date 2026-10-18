package responses

import "strings"

// HasRejectedNativeToolMarkup detects a complete native DSML call block that
// slipped through the gateway's tool-call repair. When a Responses request
// declares tools, returning such a block as assistant text falsely tells
// clients (Codex Desktop included) that the turn completed without executing
// any tools. Detect the exact native call delimiters rather than generic
// mentions of the word DSML, so documentation/examples do not trigger it.
func HasRejectedNativeToolMarkup(content string) bool {
	const p = "\uFF5C\uFF5C"
	return strings.Contains(content, "<"+p+"DSML"+p+" calls>") &&
		strings.Contains(content, "<"+p+"DSML"+p+" invoke ")
}
