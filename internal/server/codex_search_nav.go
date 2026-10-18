package server

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type codexOpenedPage struct {
	At    time.Time
	URL   string
	Title string
	Lines []string
	Links []string
}

var (
	codexHTMLScripts = regexp.MustCompile("(?is)<(?:script|style|svg|noscript)\\b[^>]*>.*?</(?:script|style|svg|noscript)\\s*>")
	codexHTMLHead    = regexp.MustCompile("(?is)<head\\b[^>]*>.*?</head\\s*>")
	codexHTMLTags    = regexp.MustCompile("(?is)<[^>]*>")
	codexHTMLBreaks  = regexp.MustCompile("(?is)<(?:br|hr)\\b[^>]*>|</(?:p|div|li|section|article|h[1-6]|tr|td|pre|blockquote)\\s*>")
	codexHTMLAnchor  = regexp.MustCompile("(?is)<a\\b[^>]*\\bhref\\s*=\\s*(?:\"([^\"]*)\"|'([^']*)'|([^\\s>]+))")
	codexHTMLTitle   = regexp.MustCompile("(?is)<title\\b[^>]*>(.*?)</title\\s*>")
)

func parseCodexHTML(raw, rawURL string) *codexOpenedPage {
	root, _ := url.Parse(rawURL)
	title := ""
	if match := codexHTMLTitle.FindStringSubmatch(raw); len(match) > 1 {
		title = strings.TrimSpace(html.UnescapeString(codexHTMLTags.ReplaceAllString(match[1], "")))
	}
	links := []string{}
	seen := map[string]bool{}
	for _, m := range codexHTMLAnchor.FindAllStringSubmatch(raw, 100) {
		href := m[1] + m[2] + m[3]
		u, err := url.Parse(strings.TrimSpace(html.UnescapeString(href)))
		if err != nil || root == nil {
			continue
		}
		abs := root.ResolveReference(u)
		if _, err := safeSearchURL(abs.String()); err != nil {
			continue
		}
		v := abs.String()
		if seen[v] {
			continue
		}
		seen[v] = true
		links = append(links, v)
		if len(links) >= 40 {
			break
		}
	}
	clean := codexHTMLScripts.ReplaceAllString(raw, "")
	clean = codexHTMLHead.ReplaceAllString(clean, "")
	clean = codexHTMLBreaks.ReplaceAllString(clean, "\n")
	clean = html.UnescapeString(codexHTMLTags.ReplaceAllString(clean, " "))
	clean = strings.ReplaceAll(clean, "\r", "\n")
	lines := []string{}
	for _, line := range strings.Split(clean, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			runes := []rune(line)
			if len(runes) > 320 {
				line = string(runes[:320]) + "..."
			}
			lines = append(lines, line)
			if len(lines) >= 160 {
				break
			}
		}
	}
	return &codexOpenedPage{At: time.Now(), URL: rawURL, Title: title, Lines: lines, Links: links}
}

func (h *Handler) codexFetchPage(ctx context.Context, rawURL string) (*codexOpenedPage, error) {
	target, err := safeSearchURL(rawURL)
	if err != nil {
		return nil, err
	}
	target.Fragment = ""
	key := target.String()
	h.codexRefs.mu.Lock()
	if p, ok := h.codexRefs.pages[key]; ok && time.Since(p.At) < 10*time.Minute {
		h.codexRefs.mu.Unlock()
		return p, nil
	}
	h.codexRefs.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "WorkBuddy2API-CodexSearch/1.0")
	req.Header.Set("Accept", "text/html,text/plain;q=0.9")
	resp, err := codexPageClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/html") && !strings.Contains(ct, "text/plain") {
		return nil, fmt.Errorf("page is not HTML/text")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	page := parseCodexHTML(string(data), resp.Request.URL.String())
	h.codexRefs.mu.Lock()
	if h.codexRefs.pages == nil {
		h.codexRefs.pages = make(map[string]*codexOpenedPage)
	}
	if len(h.codexRefs.pages) > 48 {
		var oldest string
		var oldestAt time.Time
		for k, p := range h.codexRefs.pages {
			if oldest == "" || p.At.Before(oldestAt) {
				oldest, oldestAt = k, p.At
			}
		}
		delete(h.codexRefs.pages, oldest)
	}
	h.codexRefs.pages[key] = page
	h.codexRefs.mu.Unlock()
	return page, nil
}

func (h *Handler) codexOpenPage(ctx context.Context, session, ref string, lineno int) (string, error) {
	target, ok := h.codexRefs.resolve(session, ref)
	if !ok {
		return "", fmt.Errorf("reference expired or missing; search again or pass a public URL")
	}
	page, err := h.codexFetchPage(ctx, target)
	if err != nil {
		return "", err
	}
	first := lineno
	if first < 0 {
		first = 0
	}
	if first >= len(page.Lines) {
		first = 0
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Open %s (%s) ref %s\n", page.Title, page.URL, ref)
	end := first + 45
	if end > len(page.Lines) {
		end = len(page.Lines)
	}
	for i := first; i < end; i++ {
		fmt.Fprintf(&out, "L%d: %s\n", i, page.Lines[i])
	}
	if end < len(page.Lines) {
		fmt.Fprintf(&out, "... additional lines available from lineno=%d\n", end)
	}
	if first == 0 && len(page.Links) > 0 {
		out.WriteString("Links (use click with id):\n")
		for i, link := range page.Links {
			fmt.Fprintf(&out, "[%d] %s\n", i+1, link)
		}
	}
	return out.String(), nil
}

func (h *Handler) codexFindPage(ctx context.Context, session, ref, pattern string) (string, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("find pattern is empty")
	}
	target, ok := h.codexRefs.resolve(session, ref)
	if !ok {
		return "", fmt.Errorf("reference expired or missing")
	}
	page, err := h.codexFetchPage(ctx, target)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Find %q in %s\n", pattern, page.URL)
	n := 0
	for i, line := range page.Lines {
		if strings.Contains(strings.ToLower(line), strings.ToLower(pattern)) {
			fmt.Fprintf(&out, "L%d: %s\n", i, line)
			n++
			if n >= 15 {
				break
			}
		}
	}
	if n == 0 {
		out.WriteString("No matches.\n")
	}
	return out.String(), nil
}

func (h *Handler) codexClickPage(ctx context.Context, session, ref string, id int) (string, error) {
	target, ok := h.codexRefs.resolve(session, ref)
	if !ok {
		return "", fmt.Errorf("reference expired or missing")
	}
	page, err := h.codexFetchPage(ctx, target)
	if err != nil {
		return "", err
	}
	if id < 1 || id > len(page.Links) {
		return "", fmt.Errorf("link id %d is not listed on this page; open the page first", id)
	}
	return h.codexOpenPage(ctx, session, page.Links[id-1], 0)
}
