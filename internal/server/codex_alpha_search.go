package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Codex CLI's provider-relative POST alpha/search protocol. The response schema
// (output, results, encrypted_output) follows codex-api/src/search.rs.
const codexSearchBodyLimit = 1 << 20

type codexSearchRequest struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Commands struct {
		SearchQuery []struct {
			Q       string   `json:"q"`
			Recency *int     `json:"recency"`
			Domains []string `json:"domains"`
		} `json:"search_query"`
		ImageQuery json.RawMessage `json:"image_query"`
		Open       []struct {
			RefID  string `json:"ref_id"`
			LineNo int    `json:"lineno"`
		} `json:"open"`
		Find []struct {
			RefID   string `json:"ref_id"`
			Pattern string `json:"pattern"`
		} `json:"find"`
		Click []struct {
			RefID string `json:"ref_id"`
			ID    int    `json:"id"`
		} `json:"click"`
		Screenshot json.RawMessage `json:"screenshot"`
		Finance    json.RawMessage `json:"finance"`
		Weather    json.RawMessage `json:"weather"`
		Sports     json.RawMessage `json:"sports"`
		Time       json.RawMessage `json:"time"`
	} `json:"commands"`
	Settings struct {
		Filters struct {
			AllowedDomains []string `json:"allowed_domains"`
			BlockedDomains []string `json:"blocked_domains"`
		} `json:"filters"`
	} `json:"settings"`
}

type codexSearchResponse struct {
	EncryptedOutput *string          `json:"encrypted_output"`
	Output          string           `json:"output"`
	Results         []map[string]any `json:"results"`
}

type codexSearchReference struct {
	URL, Title, Snippet string
}
type codexSessionRefs struct {
	At   time.Time
	Next int
	Refs map[string]codexSearchReference
}
type codexSearchIndex struct {
	mu       sync.Mutex
	sessions map[string]*codexSessionRefs
	pages    map[string]*codexOpenedPage
}

func (x *codexSearchIndex) store(session string, rows []upstream.SearchResult) []map[string]any {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.sessions == nil {
		x.sessions = make(map[string]*codexSessionRefs)
	}
	now := time.Now()
	for id, s := range x.sessions {
		if now.Sub(s.At) > 20*time.Minute {
			delete(x.sessions, id)
		}
	}
	if len(x.sessions) > 128 {
		var oldest string
		var oldestAt time.Time
		for id, s := range x.sessions {
			if oldest == "" || s.At.Before(oldestAt) {
				oldest, oldestAt = id, s.At
			}
		}
		delete(x.sessions, oldest)
	}
	s := x.sessions[session]
	if s == nil {
		s = &codexSessionRefs{Refs: make(map[string]codexSearchReference)}
		x.sessions[session] = s
	}
	s.At = now
	result := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if len(s.Refs) > 200 {
			s.Refs = make(map[string]codexSearchReference)
		}
		ref := fmt.Sprintf("turn0search%d", s.Next)
		s.Next++
		s.Refs[ref] = codexSearchReference{URL: row.URL, Title: row.Title, Snippet: row.Snippet}
		result = append(result, map[string]any{"type": "text_result", "ref_id": ref, "url": row.URL, "title": row.Title, "snippet": row.Snippet})
	}
	return result
}

func (x *codexSearchIndex) resolve(session, ref string) (string, bool) {
	// Literal URLs can be opened even after gateway restart; ref ids only
	// resolve in their authenticated Codex session.
	if u, err := url.Parse(ref); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" {
		return ref, true
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	s := x.sessions[session]
	if s == nil || time.Since(s.At) > 20*time.Minute {
		return "", false
	}
	e, ok := s.Refs[ref]
	return e.URL, ok
}

func (h *Handler) codexAlphaSearch(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, codexSearchBodyLimit+1))
	if err != nil || len(raw) > codexSearchBodyLimit {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "search request too large or unreadable")
		return
	}
	var req codexSearchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid search JSON: "+err.Error())
		return
	}
	if req.ID == "" {
		req.ID = "anon-" + randomHex(12)
	}
	if len(req.ID) > 256 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "search session id too long")
		return
	}
	nops := len(req.Commands.SearchQuery) + len(req.Commands.Open) + len(req.Commands.Find) + len(req.Commands.Click)
	hasUnsupported := hasNonEmptyCommand(req.Commands.ImageQuery) || hasNonEmptyCommand(req.Commands.Screenshot) ||
		hasNonEmptyCommand(req.Commands.Finance) || hasNonEmptyCommand(req.Commands.Weather) ||
		hasNonEmptyCommand(req.Commands.Sports) || hasNonEmptyCommand(req.Commands.Time)
	if (nops == 0 && !hasUnsupported) || nops > 12 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "expected 1 to 12 search/open/find/click operations")
		return
	}
	resp := codexSearchResponse{Output: "", Results: []map[string]any{}}
	var output strings.Builder
	meta := searchMetaFromRequest(r)
	for _, q := range req.Commands.SearchQuery {
		query := strings.TrimSpace(q.Q)
		if len([]rune(query)) < upstream.SearchMinQueryLen {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "search query must have at least 2 characters")
			return
		}
		out, fail := h.runSearch(r.Context(), query, 0, meta)
		if fail != nil {
			writeOpenAIErrorHint(w, fail.status, "upstream_search_failed", fail.msg, fail.hint)
			return
		}
		filtered := make([]upstream.SearchResult, 0, len(out.Results))
		for _, row := range out.Results {
			// Query-level domains and global allowed_domains are an
			// intersection, not a union; blocked_domains always excludes.
			if isPermittedSearchDomain(row.URL, req.Settings.Filters.AllowedDomains, req.Settings.Filters.BlockedDomains) &&
				isPermittedSearchDomain(row.URL, q.Domains, nil) {
				filtered = append(filtered, row)
			}
		}
		results := h.codexRefs.store(req.ID, filtered)
		resp.Results = append(resp.Results, results...)
		fmt.Fprintf(&output, "Search results for %q:\n", query)
		for i, row := range filtered {
			title := row.Title
			if title == "" {
				title = row.URL
			}
			fmt.Fprintf(&output, "%d. %s (%s) [%s]\n", i+1, title, row.URL, results[i]["ref_id"])
			if row.Snippet != "" {
				fmt.Fprintf(&output, "   %s\n", row.Snippet)
			}
		}
		if len(filtered) == 0 {
			output.WriteString("No results returned for this query or matching the requested domains.\n")
		}
		if q.Recency != nil {
			output.WriteString("Note: date/recency filtering is not supported by this search backend; results are not date-filtered.\n")
		}
		output.WriteByte('\n')
	}
	for _, v := range req.Commands.Open {
		text, e := h.codexOpenPage(r.Context(), req.ID, v.RefID, v.LineNo)
		if e != nil {
			fmt.Fprintf(&output, "Open %s: %v\n", v.RefID, e)
		} else {
			output.WriteString(text)
		}
	}
	for _, v := range req.Commands.Find {
		text, e := h.codexFindPage(r.Context(), req.ID, v.RefID, v.Pattern)
		if e != nil {
			fmt.Fprintf(&output, "Find %s: %v\n", v.RefID, e)
		} else {
			output.WriteString(text)
		}
	}
	for _, v := range req.Commands.Click {
		text, e := h.codexClickPage(r.Context(), req.ID, v.RefID, v.ID)
		if e != nil {
			fmt.Fprintf(&output, "Click %s: %v\n", v.RefID, e)
		} else {
			output.WriteString(text)
		}
	}
	if hasUnsupported {
		output.WriteString("Unsupported operation: this WorkBuddy search backend supports web text search and safe web page navigation, not image/PDF screenshot or specialized finance/weather/sports/time tools.\n")
	}
	resp.Output = strings.TrimSpace(output.String())
	writeJSON(w, http.StatusOK, resp)
}

func hasNonEmptyCommand(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null" && string(raw) != "[]" && string(raw) != "{}"
}

func isPermittedSearchDomain(raw string, allowed, blocked []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	match := func(d string) bool {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(d), "*."), "."))
		return d != "" && (host == d || strings.HasSuffix(host, "."+d))
	}
	for _, d := range blocked {
		if match(d) {
			return false
		}
	}
	if len(allowed) > 0 {
		for _, d := range allowed {
			if match(d) {
				return true
			}
		}
		return false
	}
	return true
}

// Prevent SSRF when Codex requests open/click/find. Literal and stored URLs
// undergo the same verification; the dialer connects only to validated public
// DNS/IP addresses and never uses environment proxies.
func safeSearchURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("only public HTTP(S) URLs can be opened")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return nil, fmt.Errorf("nonstandard ports are not allowed")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("local/private hosts are not allowed")
	}
	return u, nil
}

func publicSearchIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified() &&
		!ipInCIDRs(ip, []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001:db8::/32", "fc00::/7", "fe80::/10"})
}
func ipInCIDRs(ip net.IP, cidrs []string) bool {
	for _, s := range cidrs {
		_, cidr, _ := net.ParseCIDR(s)
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var codexPageClient = &http.Client{
	Timeout: 9 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if publicSearchIP(ip.IP) {
					return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				}
			}
			return nil, fmt.Errorf("address has no public IP")
		},
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: 2,
		DisableKeepAlives:   true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		_, err := safeSearchURL(req.URL.String())
		return err
	},
}
