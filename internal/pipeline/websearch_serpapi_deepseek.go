// Package pipeline - SerpApi and DeepSeek web-search tools.
// Ported from astrbot/core/tools/web_search_tools.py (4.29.1 / 4.29.4):
// SerpApi Web/News/Image/Finance/Scholar + DeepSeek Web/Fetch.
package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
)

// ---------------------------------------------------------------------------
// Keys & shared request helpers
// ---------------------------------------------------------------------------

func serpapiKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_serpapi_key")
}

func deepseekKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_deepseek_key")
}

// shortRefID returns a 4-hex-char reference id (aligned with py uuid4()[:4]).
func shortRefID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// quoteSafe percent-encodes s while leaving the characters in safe untouched
// (aligned with Python urllib.parse.quote(s, safe=safe)).
func quoteSafe(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func jsonStr(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func jsonText(v interface{}) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// clampInt constrains v to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// serpapiRequest calls a SerpApi search engine with API key failover.
func serpapiRequest(cfg map[string]interface{}, payload map[string]interface{}, engine string) (map[string]interface{}, error) {
	keys := serpapiKeys(cfg)
	if len(keys) == 0 {
		return nil, fmt.Errorf("Error: SerpApi API key is not configured in AstrBot.")
	}
	var lastErr error
	for _, apiKey := range keys {
		q := make(map[string]interface{}, len(payload)+2)
		for k, v := range payload {
			q[k] = v
		}
		q["engine"] = engine
		q["api_key"] = apiKey

		data, status, err := httpJSON(http.MethodGet, "https://serpapi.com/search.json", nil, q)
		if err != nil {
			// Transport errors can include the request URL with its API key.
			return nil, fmt.Errorf("SerpApi search request failed or timed out.")
		}
		if status == 200 {
			var decoded map[string]interface{}
			if err := json.Unmarshal(data, &decoded); err != nil {
				return nil, fmt.Errorf("SerpApi search returned invalid JSON.")
			}
			md, _ := decoded["search_metadata"].(map[string]interface{})
			mdStatus, _ := md["status"].(string)
			if mdStatus == "Error" || (decoded["error"] != nil && mdStatus != "Success") {
				reason := jsonStr(decoded["error"])
				if reason == "" {
					reason = "Unknown API error."
				}
				return nil, fmt.Errorf("SerpApi search failed: %s", strings.ReplaceAll(reason, apiKey, "[redacted]"))
			}
			return decoded, nil
		}
		lastErr = fmt.Errorf("SerpApi search failed: %s, status: %d",
			strings.ReplaceAll(string(data), apiKey, "[redacted]"), status)
		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests {
			continue
		}
		return nil, lastErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("SerpApi search failed with all configured keys.")
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

func serpApiWebSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_serpapi",
		"Search the web using SerpApi Google Search for current information. Returns page titles, URLs, and snippets.",
		map[string]interface{}{
			"max_results": map[string]interface{}{"type": "integer", "description": "Maximum results to return from the first page, 1-10. Default is 5; fewer may be available."},
			"country":     map[string]interface{}{"type": "string", "description": "Optional. Google country code, such as us, uk, or cn."},
			"search_lang": map[string]interface{}{"type": "string", "description": "Optional. Google interface language code, such as en or zh-cn."},
			"location":    map[string]interface{}{"type": "string", "description": "Optional. Search origin, such as Austin, Texas, United States."},
		})
}

func serpApiNewsSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("news_search_serpapi",
		"Search current news using the SerpApi Google News API. Returns article titles, source URLs, publishers, authors, snippets, and publication times.",
		map[string]interface{}{
			"max_results": map[string]interface{}{"type": "integer", "description": "Maximum articles to return, 1-10. Default is 5; fewer may be available."},
			"country":     map[string]interface{}{"type": "string", "description": "Optional. Google News country code, such as us, uk, or cn."},
			"search_lang": map[string]interface{}{"type": "string", "description": "Optional. Google News language code, such as en or zh-cn."},
		})
}

func serpApiImageSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("image_search_serpapi",
		"Find images using SerpApi Google Images Light. Returns direct image URLs, thumbnails, dimensions, and source page URLs. Use image_url to display or send an image, and url to cite its source page.",
		map[string]interface{}{
			"max_results": map[string]interface{}{"type": "integer", "description": "Maximum images to return, 1-10. Default is 5; fewer may be available."},
			"country":     map[string]interface{}{"type": "string", "description": "Optional. Google country code, such as us, uk, or cn."},
			"search_lang": map[string]interface{}{"type": "string", "description": "Optional. Google interface language code, such as en or zh-cn."},
		})
}

func serpApiFinanceSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("finance_search_serpapi",
		"Look up stocks, indices, funds, currencies, and futures using Google Finance. Use ticker symbols such as AAPL:NASDAQ or currency pairs such as EUR-USD. Returns quote prices, currency, timestamps, key statistics, financials, and news when available. Set window only when price history is needed.",
		map[string]interface{}{
			"search_lang": map[string]interface{}{"type": "string", "description": "Optional. Google interface language code, such as en or zh-cn."},
			"window":      map[string]interface{}{"type": "string", "enum": []interface{}{"1D", "5D", "1M", "6M", "YTD", "1Y", "5Y", "MAX"}, "description": "Optional. Price history range. When set, Google Finance returns a quote summary and graph instead of full financials and news."},
		})
}

func serpApiScholarSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("scholar_search_serpapi",
		"Search academic publications using Google Scholar. Returns titles, source URLs, snippets, publication details, citation counts, and available PDF or full-text links. Supports publication year filters.",
		map[string]interface{}{
			"max_results": map[string]interface{}{"type": "integer", "description": "Maximum publications to return, 1-20. Default is 5."},
			"search_lang": map[string]interface{}{"type": "string", "description": "Optional. Google Scholar interface language, such as en or zh-cn."},
			"year_from":   map[string]interface{}{"type": "integer", "description": "Optional. Earliest publication year, inclusive."},
			"year_to":     map[string]interface{}{"type": "integer", "description": "Optional. Latest publication year, inclusive."},
		})
}

func deepseekWebSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_deepseek",
		"Search the web using DeepSeek and return summaries with source references. Use deepseek_fetch_web_page for full page content when needed.",
		map[string]interface{}{
			"count":        map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 50, "description": "Optional. Number of results, 1-50. Default is 10."},
			"ori_question": map[string]interface{}{"type": "string", "description": "Optional. Original user question, 1-128 Unicode characters."},
			"need_content": map[string]interface{}{"type": "boolean", "description": "Optional. Include full page content in results. Default is false."},
		})
}

func deepseekFetchToolSchema() map[string]interface{} {
	return urlToolSchema("deepseek_fetch_web_page",
		"Fetch a single web page using DeepSeek and return its title and Markdown content. Use this to read a known URL, including a page found through web search.",
		nil)
}

// ---------------------------------------------------------------------------
// SerpApi executors
// ---------------------------------------------------------------------------

func executeWebSearchSerpApi(cfg map[string]interface{}, args map[string]interface{}) string {
	if len(serpapiKeys(cfg)) == 0 {
		return "Error: SerpApi API key is not configured in AstrBot."
	}
	query := argString(args, "query")
	if strings.TrimSpace(query) == "" {
		return "Error: web_search_serpapi requires a query."
	}
	maxResults := clampInt(argInt(args, "max_results", 5), 1, 10)
	payload := map[string]interface{}{"q": query}
	if v := argString(args, "country"); v != "" {
		payload["gl"] = v
	}
	if v := argString(args, "search_lang"); v != "" {
		payload["hl"] = v
	}
	if v := argString(args, "location"); v != "" {
		payload["location"] = v
	}
	data, err := serpapiRequest(cfg, payload, "google")
	if err != nil {
		return "Error: " + err.Error()
	}
	items, _ := data["organic_results"].([]interface{})
	results := make([]searchResult, 0, maxResults)
	for _, item := range items {
		m, _ := item.(map[string]interface{})
		link := jsonStr(m["link"])
		if link == "" {
			continue
		}
		results = append(results, searchResult{Title: jsonStr(m["title"]), URL: link, Snippet: jsonStr(m["snippet"])})
		if len(results) >= maxResults {
			break
		}
	}
	return formatSearchResults(results)
}

func executeNewsSearchSerpApi(cfg map[string]interface{}, args map[string]interface{}) string {
	if len(serpapiKeys(cfg)) == 0 {
		return "Error: SerpApi API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: query must not be empty."
	}
	maxResults := clampInt(argInt(args, "max_results", 5), 1, 10)
	payload := map[string]interface{}{"q": query}
	if v := argString(args, "country"); v != "" {
		payload["gl"] = v
	}
	if v := argString(args, "search_lang"); v != "" {
		payload["hl"] = v
	}
	data, err := serpapiRequest(cfg, payload, "google_news")
	if err != nil {
		return "Error: " + err.Error()
	}

	refUUID := shortRefID()
	results := []map[string]interface{}{}
	seen := map[string]struct{}{}
	groups, _ := data["news_results"].([]interface{})
	for _, g := range groups {
		group, _ := g.(map[string]interface{})
		articles := []map[string]interface{}{}
		if jsonStr(group["link"]) != "" {
			articles = append(articles, group)
		}
		if highlight, ok := group["highlight"].(map[string]interface{}); ok {
			articles = append(articles, highlight)
		}
		if stories, ok := group["stories"].([]interface{}); ok {
			for _, s := range stories {
				if sm, ok := s.(map[string]interface{}); ok {
					articles = append(articles, sm)
				}
			}
		}
		for _, article := range articles {
			sourceURL := jsonStr(article["link"])
			if sourceURL == "" {
				continue
			}
			if _, dup := seen[sourceURL]; dup {
				continue
			}
			seen[sourceURL] = struct{}{}
			source, _ := article["source"].(map[string]interface{})
			results = append(results, map[string]interface{}{
				"title":         jsonStr(article["title"]),
				"url":           sourceURL,
				"snippet":       jsonStr(article["snippet"]),
				"source":        orEmpty(jsonStr(source["name"]), jsonStr(source["title"])),
				"authors":       source["authors"],
				"published_at":  orEmpty(jsonStr(article["iso_date"]), jsonStr(article["date"])),
				"thumbnail_url": orEmpty(jsonStr(article["thumbnail"]), jsonStr(article["thumbnail_small"])),
				"index":         fmt.Sprintf("%s.%d", refUUID, len(results)+1),
			})
			if len(results) >= maxResults {
				break
			}
		}
		if len(results) >= maxResults {
			break
		}
	}
	return jsonText(map[string]interface{}{"results": results})
}

func executeImageSearchSerpApi(cfg map[string]interface{}, args map[string]interface{}) string {
	if len(serpapiKeys(cfg)) == 0 {
		return "Error: SerpApi API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: query must not be empty."
	}
	maxResults := clampInt(argInt(args, "max_results", 5), 1, 10)
	payload := map[string]interface{}{"q": query}
	if v := argString(args, "country"); v != "" {
		payload["gl"] = v
	}
	if v := argString(args, "search_lang"); v != "" {
		payload["hl"] = v
	}
	data, err := serpapiRequest(cfg, payload, "google_images_light")
	if err != nil {
		return "Error: " + err.Error()
	}
	refUUID := shortRefID()
	results := []map[string]interface{}{}
	items, _ := data["images_results"].([]interface{})
	for _, item := range items {
		m, _ := item.(map[string]interface{})
		original := jsonStr(m["original"])
		imageURL := original
		thumbnailURL := orEmpty(jsonStr(m["thumbnail"]), jsonStr(m["serpapi_thumbnail"]))
		// Images extracted from PDFs can use x-raw-image:// references; fall back
		// to the HTTP thumbnail when no direct HTTP image URL is available.
		if !isHTTPURL(imageURL) {
			imageURL = thumbnailURL
		}
		if !isHTTPURL(imageURL) {
			continue
		}
		sourceURL := orEmpty(jsonStr(m["link"]), imageURL)
		entry := map[string]interface{}{
			"title":         jsonStr(m["title"]),
			"url":           sourceURL,
			"snippet":       jsonStr(m["source"]),
			"image_url":     imageURL,
			"thumbnail_url": thumbnailURL,
			"index":         fmt.Sprintf("%s.%d", refUUID, len(results)+1),
		}
		if imageURL == original && original != "" {
			entry["width"] = m["original_width"]
			entry["height"] = m["original_height"]
		} else {
			entry["width"] = nil
			entry["height"] = nil
		}
		results = append(results, entry)
		if len(results) >= maxResults {
			break
		}
	}
	return jsonText(map[string]interface{}{"results": results})
}

func executeFinanceSearchSerpApi(cfg map[string]interface{}, args map[string]interface{}) string {
	if len(serpapiKeys(cfg)) == 0 {
		return "Error: SerpApi API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: query must not be empty."
	}
	window := argString(args, "window")
	validWindows := map[string]bool{"1D": true, "5D": true, "1M": true, "6M": true, "YTD": true, "1Y": true, "5Y": true, "MAX": true}
	if window != "" && !validWindows[window] {
		return "Error: window must be one of 1D, 5D, 1M, 6M, YTD, 1Y, 5Y, MAX."
	}
	payload := map[string]interface{}{"q": query}
	if window != "" {
		payload["window"] = window
	}
	if v := argString(args, "search_lang"); v != "" {
		payload["hl"] = v
	}
	data, err := serpapiRequest(cfg, payload, "google_finance")
	if err != nil {
		return "Error: " + err.Error()
	}
	summary, _ := data["summary"].(map[string]interface{})
	result := map[string]interface{}{
		"results":         []map[string]interface{}{},
		"summary":         summary,
		"knowledge_graph": data["knowledge_graph"],
		"financials":      data["financials"],
		"news_results":    data["news_results"],
		"suggestions":     data["suggestions"],
	}
	if len(summary) > 0 {
		md, _ := data["search_metadata"].(map[string]interface{})
		sourceURL := jsonStr(md["google_finance_url"])
		if sourceURL == "" {
			sourceURL = "https://www.google.com/finance/quote/" + quoteSafe(query, ":-.")
		}
		result["results"] = []map[string]interface{}{{
			"title":   orEmpty(jsonStr(summary["title"]), query),
			"url":     sourceURL,
			"snippet": jsonText(summary),
			"index":   shortRefID() + ".1",
		}}
	}
	if window != "" {
		result["graph"] = data["graph"]
		result["key_events"] = data["key_events"]
	}
	return jsonText(result)
}

func executeScholarSearchSerpApi(cfg map[string]interface{}, args map[string]interface{}) string {
	if len(serpapiKeys(cfg)) == 0 {
		return "Error: SerpApi API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: query must not be empty."
	}
	maxResults := clampInt(argInt(args, "max_results", 5), 1, 20)
	payload := map[string]interface{}{"q": query, "num": maxResults}
	if _, ok := args["year_from"]; ok {
		year := argInt(args, "year_from", 0)
		if year < 1 || year > 9999 {
			return "Error: publication years must be between 1 and 9999."
		}
		payload["as_ylo"] = year
	}
	if _, ok := args["year_to"]; ok {
		year := argInt(args, "year_to", 0)
		if year < 1 || year > 9999 {
			return "Error: publication years must be between 1 and 9999."
		}
		payload["as_yhi"] = year
	}
	ylo := 1
	yhi := 9999
	if v, ok := payload["as_ylo"]; ok {
		ylo = v.(int)
	}
	if v, ok := payload["as_yhi"]; ok {
		yhi = v.(int)
	}
	if ylo > yhi {
		return "Error: year_from must not be greater than year_to."
	}
	if v := argString(args, "search_lang"); v != "" {
		payload["hl"] = v
	}
	data, err := serpapiRequest(cfg, payload, "google_scholar")
	if err != nil {
		return "Error: " + err.Error()
	}
	refUUID := shortRefID()
	results := []map[string]interface{}{}
	items, _ := data["organic_results"].([]interface{})
	for _, item := range items {
		m, _ := item.(map[string]interface{})
		sourceURL := jsonStr(m["link"])
		if sourceURL == "" {
			if resources, ok := m["resources"].([]interface{}); ok {
				for _, r := range resources {
					rm, _ := r.(map[string]interface{})
					if link := jsonStr(rm["link"]); link != "" {
						sourceURL = link
						break
					}
				}
			}
		}
		if sourceURL == "" {
			sourceURL = "https://scholar.google.com/scholar?q=" + quoteSafe(jsonStr(m["title"]), "")
		}
		citedBy, _ := m["inline_links"].(map[string]interface{})
		citedByMap, _ := citedBy["cited_by"].(map[string]interface{})
		results = append(results, map[string]interface{}{
			"title":            jsonStr(m["title"]),
			"url":              sourceURL,
			"snippet":          jsonStr(m["snippet"]),
			"publication_info": m["publication_info"],
			"cited_by_count":   citedByMap["total"],
			"cited_by_url":     citedByMap["link"],
			"resources":        m["resources"],
			"index":            fmt.Sprintf("%s.%d", refUUID, len(results)+1),
		})
		if len(results) >= maxResults {
			break
		}
	}
	return jsonText(map[string]interface{}{"results": results})
}

// ---------------------------------------------------------------------------
// DeepSeek executors
// ---------------------------------------------------------------------------

func executeWebSearchDeepSeek(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := deepseekKeys(cfg)
	if len(keys) == 0 {
		return "Error: DeepSeek API key is not configured in AstrBot."
	}
	query := argString(args, "query")
	if strings.TrimSpace(query) == "" || len([]rune(query)) > 128 {
		return "Error: query must be a non-blank string of 1-128 Unicode characters."
	}
	count := argInt(args, "count", 10)
	if count < 1 || count > 50 {
		return "Error: count must be an integer between 1 and 50."
	}
	oriQuestion := argString(args, "ori_question")
	if oriQuestion != "" && len([]rune(oriQuestion)) > 128 {
		return "Error: ori_question must be a string of 1-128 Unicode characters."
	}
	needContent := argBoolDefault(args, "need_content", false)

	payload := map[string]interface{}{"query": query, "count": count, "need_content": needContent}
	if oriQuestion != "" {
		payload["ori_question"] = oriQuestion
	}
	data, err := deepseekPost(keys[0], "https://api.deepseek.com/v1/tools/search", payload)
	if err != nil {
		return "Error: " + err.Error()
	}
	items, _ := data["results"].([]interface{})
	results := make([]searchResult, 0, len(items))
	for _, item := range items {
		m, _ := item.(map[string]interface{})
		snippet := orEmpty(jsonStr(m["summary"]), jsonStr(m["snippet"]))
		if needContent && jsonStr(m["content"]) != "" {
			snippet = strings.Join(nonEmptyStrings(snippet, jsonStr(m["content"])), "\n\n")
		}
		results = append(results, searchResult{
			Title:   jsonStr(m["title"]),
			URL:     jsonStr(m["url"]),
			Snippet: snippet,
		})
	}
	if len(results) == 0 {
		return "Error: DeepSeek web search does not return any results."
	}
	return formatSearchResults(results)
}

func executeDeepSeekFetch(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := deepseekKeys(cfg)
	if len(keys) == 0 {
		return "Error: DeepSeek API key is not configured in AstrBot."
	}
	url := argString(args, "url")
	if url == "" || len(url) > 8192 || !validFetchURL(url) {
		return "Error: url must be an HTTP or HTTPS URL with a host and no embedded credentials."
	}
	data, err := deepseekPost(keys[0], "https://api.deepseek.com/v1/tools/fetch", map[string]interface{}{"url": url})
	if err != nil {
		return "Error: " + err.Error()
	}
	content := jsonStr(data["content"])
	if content == "" {
		return "Error: DeepSeek fetch does not return any content."
	}
	return fmt.Sprintf("URL: %s\nTitle: %s\nContent: %s", url, jsonStr(data["title"]), content)
}

// deepseekPost POSTs a JSON payload to the DeepSeek tools API.
func deepseekPost(apiKey, endpoint string, payload map[string]interface{}) (map[string]interface{}, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Content-Type":  "application/json",
	}
	data, status, err := httpJSON(http.MethodPost, endpoint, headers, payload)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("DeepSeek request failed: %s, status: %d", string(data), status)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("DeepSeek request returned invalid JSON.")
	}
	return decoded, nil
}

func nonEmptyStrings(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// validFetchURL validates an HTTP(S) URL with a host and no embedded credentials
// (aligned with py DeepSeekFetchWebPageTool validation).
func validFetchURL(raw string) bool {
	if strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}
