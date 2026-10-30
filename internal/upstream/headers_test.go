package upstream

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersAlwaysSendConversationTrio 回归 issue #190：国际端点收紧通道
// 校验后，缺 X-Conversation-ID 的聊天请求会 400 / 11128（conversation id
// missing）。即使调用方没给会话 ID（零值 ChatMeta），三元组也必须齐全，
// 且兜底 ID 是 32 位 hex。
func TestChatHeadersAlwaysSendConversationTrio(t *testing.T) {
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})

	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, h := range []string{
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	} {
		v := req.Header.Get(h)
		if v == "" {
			t.Fatalf("%s missing (通道校验会拒绝缺少会话三元组的请求)", h)
		}
		if !hex32.MatchString(v) {
			t.Errorf("%s = %q, want 32 hex", h, v)
		}
	}

	// 兜底会话 ID 每次请求新生成（不是常量），且与 request/message ID 不冲突。
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req2, testAuth(), "", ChatMeta{})
	if req.Header.Get("X-Conversation-ID") == req2.Header.Get("X-Conversation-ID") {
		t.Error("fallback X-Conversation-ID should be fresh per request")
	}
}

// TestChatHeadersPreservesConversationID 入站带来的会话 ID 优先透传，不被兜底覆盖。
func TestChatHeadersPreservesConversationID(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	const conv = "0123456789abcdef0123456789abcdef"
	c.ChatHeaders(req, testAuth(), "", ChatMeta{ConversationID: conv})
	if got := req.Header.Get("X-Conversation-ID"); got != conv {
		t.Errorf("X-Conversation-ID = %q, want supplied %q", got, conv)
	}
}

// testAuth 返回最小的 CN 账号（ChatHeaders 会读 UID/凭证，nil 会解引用）。
func testAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at"}
}

// A newer, independently observed client version tuple is used consistently
// in chat UA, X-IDE-Version and billing UA. The user overrides still take
// precedence; we do not fabricate an unverified 5.9.3 CLI version.
func TestOutboundVersionTuple(t *testing.T) {
	c := &Client{}
	cn := testAuth()
	intl := &auth.Auth{UID: "intl", AccessToken: "at", Domain: "www.workbuddy.ai"}
	for _, tc := range []struct {
		name string
		a    *auth.Auth
		ua   string
	}{
		{"cn", cn, "WorkBuddy/5.9.2 WorkBuddy/5.9.2 CLI/2.165.0"},
		{"global", intl, "WorkBuddy/5.9.2 workbuddy-ai/5.9.2 CLI/2.165.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
			c.ChatHeaders(req, tc.a, "", ChatMeta{})
			if got := req.Header.Get("User-Agent"); got != tc.ua {
				t.Fatalf("UA=%q want %q", got, tc.ua)
			}
			if got := req.Header.Get("X-IDE-Version"); got != "5.9.2" {
				t.Errorf("X-IDE-Version=%q", got)
			}
		})
	}
	if got := c.billingUA(); got != "WorkBuddy/5.9.2" {
		t.Fatalf("billing UA=%q", got)
	}
	custom := &Client{ClientVersion: "5.9.3", CliVersion: "2.custom"}
	if got := custom.defaultWorkBuddyUA(); got != "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.custom" {
		t.Fatalf("explicit version overrides were not respected: %q", got)
	}
}
