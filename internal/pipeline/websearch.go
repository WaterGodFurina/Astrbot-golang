package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider/sources"
)

// httpJSON performs a JSON HTTP request and returns the body bytes. For GET
// requests the payload (a string map) is appended to the URL as query
// parameters; for other methods it is serialized as the JSON body.
func httpJSON(method, url string, headers map[string]string, payload interface{}) ([]byte, int, error) {
	var body []byte
	if method == http.MethodGet {
		if m, ok := payload.(map[string]interface{}); ok && len(m) > 0 {
			u, err := neturl.Parse(url)
			if err != nil {
				return nil, 0, err
			}
			q := u.Query()
			for k, v := range m {
				q.Set(k, fmt.Sprint(v))
			}
			u.RawQuery = q.Encode()
			url = u.String()
		}
	} else if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := sources.DefaultRetryConfig()
	resp, err := sources.DoWithRetry(ctx, http.DefaultClient, func() (*http.Request, error) {
		// 每次重试都重新构造请求与 Body（http.Request.Clone 是浅拷贝，Body
		// 共享同一 reader，首次消耗后重试会发送空请求体）。
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}, cfg, "WebSearch")
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// webSearchProviderInfo reads the enabled web-search provider from
// provider_settings. Returns the provider name and whether web search is on.
func webSearchProviderInfo(cfg map[string]interface{}) (string, bool) {
	ps, ok := cfg["provider_settings"].(map[string]interface{})
	if !ok {
		return "", false
	}
	enabled, _ := ps["web_search"].(bool)
	if !enabled {
		return "", false
	}
	provider, _ := ps["websearch_provider"].(string)
	return provider, true
}

// providerStringKeys reads a provider_settings key that may be a string or a
// list of strings.
func providerStringKeys(cfg map[string]interface{}, key string) []string {
	ps, _ := cfg["provider_settings"].(map[string]interface{})
	if ps == nil {
		return nil
	}
	raw, ok := ps[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return []string{strings.TrimSpace(v)}
		}
	case []interface{}:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return v
	}
	return nil
}

// providerString reads a single-string provider_settings key.
func providerString(cfg map[string]interface{}, key string) string {
	ps, _ := cfg["provider_settings"].(map[string]interface{})
	if ps == nil {
		return ""
	}
	s, _ := ps[key].(string)
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// Tavily
// ---------------------------------------------------------------------------

func tavilyKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_tavily_key")
}

// webSearchToolSchema builds an OpenAI tool schema with a required query plus
// optional extra parameters.
func webSearchToolSchema(name, description string, extra map[string]interface{}) map[string]interface{} {
	properties := map[string]interface{}{
		"query": map[string]interface{}{"type": "string", "description": "Required. The search query."},
	}
	for k, v := range extra {
		properties[k] = v
	}
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        name,
			"description": description,
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": properties,
				"required":   []interface{}{"query"},
			},
		},
	}
}

func tavilySearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_tavily", "Search the web using Tavily. Returns recent search results with titles, URLs and snippets.",
		map[string]interface{}{
			"max_results":  map[string]interface{}{"type": "integer", "description": "Optional. Maximum number of results. Default 7, range 5-20."},
			"search_depth": map[string]interface{}{"type": "string", "enum": []interface{}{"basic", "advanced"}, "description": "Optional. Search depth. Default basic."},
			"topic":        map[string]interface{}{"type": "string", "enum": []interface{}{"general", "news"}, "description": "Optional. Search topic. Default general."},
		})
}

func bochaSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_bocha", "Search the web using BoCha. Returns recent search results with titles, URLs and snippets.",
		map[string]interface{}{
			"count":     map[string]interface{}{"type": "integer", "description": "Optional. Number of results. Default 10."},
			"freshness": map[string]interface{}{"type": "string", "description": "Optional. One of: noLimit, oneDay, oneWeek, oneMonth, oneYear."},
			"summary":   map[string]interface{}{"type": "boolean", "description": "Optional. Include a summary. Default false."},
		})
}

func braveSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_brave", "Search the web using Brave. Returns recent search results with titles, URLs and snippets.",
		map[string]interface{}{
			"count":       map[string]interface{}{"type": "integer", "description": "Optional. Number of results, 1-20. Default 10."},
			"country":     map[string]interface{}{"type": "string", "description": "Optional. Country code. Default US."},
			"search_lang": map[string]interface{}{"type": "string", "description": "Optional. Search language. Default zh-hans."},
			"freshness":   map[string]interface{}{"type": "string", "enum": []interface{}{"day", "week", "month", "year"}, "description": "Optional. Freshness window."},
		})
}

func firecrawlSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_firecrawl", "Search the web using Firecrawl. Returns recent search results with titles, URLs and snippets.",
		map[string]interface{}{
			"limit": map[string]interface{}{"type": "integer", "description": "Optional. Number of results. Default 5."},
		})
}

func baiduSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_baidu", "Search the web using Baidu AI Search. Returns search results with titles, URLs and snippets.",
		map[string]interface{}{
			"top_k": map[string]interface{}{"type": "integer", "description": "Optional. Number of results, 1-50. Default 10."},
		})
}

func exaSearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_exa", "Search the web using Exa. Returns search results with titles, URLs and snippets.",
		map[string]interface{}{
			"num_results": map[string]interface{}{"type": "integer", "description": "Optional. Number of results. Default 10."},
			"search_type": map[string]interface{}{"type": "string", "enum": []interface{}{"auto", "keyword", "neural"}, "description": "Optional. Search type. Default auto."},
		})
}

// anySearchToolSchema 对齐 Python v4.28.0 #9767 AnySearch 的 Field 定义。
func anySearchToolSchema() map[string]interface{} {
	return webSearchToolSchema("web_search_anysearch",
		"A web search tool powered by AnySearch. Supports general web search and domain-specific search over academic, code, finance, legal and security sources.",
		map[string]interface{}{
			"query":       map[string]interface{}{"type": "string", "description": "Required. Search query."},
			"max_results": map[string]interface{}{"type": "integer", "description": "Optional. The maximum number of results to return. Default is 10. Range is 1-20."},
			"tag":         map[string]interface{}{"type": "string", "description": `Optional. Domain capability tag in "{domain}.{subdomain}" form, for example "academic.paper" or "finance.news". Omit it for general web search.`},
			"zone":        map[string]interface{}{"type": "string", "description": `Optional. Result region, must be one of "cn", "intl".`},
			"language":    map[string]interface{}{"type": "string", "description": `Optional. Preferred result language, for example "zh-CN" or "en".`},
		})
}

// urlToolSchema builds an OpenAI tool schema with a required url plus optional
// extra parameters (for web-page extraction tools).
func urlToolSchema(name, description string, extra map[string]interface{}) map[string]interface{} {
	properties := map[string]interface{}{
		"url": map[string]interface{}{"type": "string", "description": "Required. The URL of the web page to extract."},
	}
	for k, v := range extra {
		properties[k] = v
	}
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        name,
			"description": description,
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": properties,
				"required":   []interface{}{"url"},
			},
		},
	}
}

func tavilyExtractToolSchema() map[string]interface{} {
	return urlToolSchema("tavily_extract_web_page", "Extract the content of a web page using Tavily.",
		map[string]interface{}{
			"extract_depth": map[string]interface{}{"type": "string", "enum": []interface{}{"basic", "advanced"}, "description": "Optional. Extract depth. Default basic."},
		})
}

func firecrawlExtractToolSchema() map[string]interface{} {
	return urlToolSchema("firecrawl_extract_web_page", "Extract the content of a web page using Firecrawl.",
		map[string]interface{}{
			"output_format":     map[string]interface{}{"type": "string", "enum": []interface{}{"markdown", "html", "rawHtml", "summary"}, "description": "Optional. Output format. Default markdown."},
			"only_main_content": map[string]interface{}{"type": "boolean", "description": "Optional. Only main content. Default true."},
		})
}

func exaContentsToolSchema() map[string]interface{} {
	return urlToolSchema("exa_get_contents", "Get the contents of a web page using Exa.",
		map[string]interface{}{
			"max_characters": map[string]interface{}{"type": "integer", "description": "Optional. Max characters of content. Default 3000."},
		})
}

func executeWebSearchTavily(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := tavilyKeys(cfg)
	if len(keys) == 0 {
		return "Error: Tavily API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: web_search_tavily requires a query."
	}
	maxResults := argIntDefault(args, "max_results", 7)
	if maxResults < 1 {
		maxResults = 1
	}
	if maxResults > 20 {
		maxResults = 20 // schema 声称 range 5-20，强制钳制防滥用放大账单
	}
	payload := map[string]interface{}{
		"query":           query,
		"max_results":     maxResults,
		"include_favicon": true,
		"search_depth":    argStringDefault(args, "search_depth", "basic"),
		"topic":           argStringDefault(args, "topic", "general"),
	}
	results, err := callSearchWithKeys("https://api.tavily.com/search", keys, "Authorization", "Bearer ",
		payload, parseTavilyResults)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatSearchResults(results)
}

func parseTavilyResults(data []byte) ([]searchResult, error) {
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var out []searchResult
	for _, r := range resp.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

func executeTavilyExtract(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := tavilyKeys(cfg)
	if len(keys) == 0 {
		return "Error: Tavily API key is not configured in AstrBot."
	}
	url := strings.TrimSpace(argString(args, "url"))
	if url == "" {
		return "Error: tavily_extract_web_page requires a url."
	}
	payload := map[string]interface{}{
		"urls":          []string{url},
		"extract_depth": argStringDefault(args, "extract_depth", "basic"),
	}
	data, status, err := httpJSON(http.MethodPost, "https://api.tavily.com/extract",
		map[string]string{"Authorization": "Bearer " + keys[0]}, payload)
	if err != nil {
		return "Error: " + err.Error()
	}
	if status != 200 {
		return fmt.Sprintf("Error: Tavily extract failed: %s (status %d)", string(data), status)
	}
	var resp struct {
		Results []struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "Error: " + err.Error()
	}
	if len(resp.Results) == 0 {
		return "Error: Tavily extract returned no results."
	}
	r := resp.Results[0]
	return fmt.Sprintf("URL: %s\nContent: %s", orEmpty(r.URL, "No URL"), orEmpty(r.RawContent, "No content"))
}

// ---------------------------------------------------------------------------
// BoCha
// ---------------------------------------------------------------------------

func bochaKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_bocha_key")
}

func executeWebSearchBocha(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := bochaKeys(cfg)
	if len(keys) == 0 {
		return "Error: BoCha API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: web_search_bocha requires a query."
	}
	payload := map[string]interface{}{
		"query":   query,
		"count":   argIntDefault(args, "count", 10),
		"summary": argBool(args, "summary"),
	}
	if v := strings.TrimSpace(argString(args, "freshness")); v != "" {
		payload["freshness"] = v
	}
	if v := strings.TrimSpace(argString(args, "include")); v != "" {
		payload["include"] = v
	}
	if v := strings.TrimSpace(argString(args, "exclude")); v != "" {
		payload["exclude"] = v
	}
	results, err := callSearchWithKeys("https://api.bochaai.com/v1/web-search", keys, "Authorization", "Bearer ",
		payload, parseBochaResults)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatSearchResults(results)
}

func parseBochaResults(data []byte) ([]searchResult, error) {
	var resp struct {
		Data struct {
			WebPages struct {
				Value []struct {
					Name     string `json:"name"`
					URL      string `json:"url"`
					Snippet  string `json:"snippet"`
					SiteIcon string `json:"siteIcon"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var out []searchResult
	for _, r := range resp.Data.WebPages.Value {
		out = append(out, searchResult{Title: r.Name, URL: r.URL, Snippet: r.Snippet})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Brave
// ---------------------------------------------------------------------------

func braveKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_brave_key")
}

func executeWebSearchBrave(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := braveKeys(cfg)
	if len(keys) == 0 {
		return "Error: Brave API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: web_search_brave requires a query."
	}
	count := argIntDefault(args, "count", 10)
	if count < 1 {
		count = 1
	}
	if count > 20 {
		count = 20
	}
	params := map[string]interface{}{
		"q":           query,
		"count":       count,
		"country":     argStringDefault(args, "country", "US"),
		"search_lang": argStringDefault(args, "search_lang", "zh-hans"),
	}
	if v := strings.TrimSpace(argString(args, "freshness")); v != "" {
		params["freshness"] = v
	}
	data, status, err := httpJSON(http.MethodGet, "https://api.search.brave.com/res/v1/web/search",
		map[string]string{"X-Subscription-Token": keys[0], "Accept": "application/json"}, params)
	if err != nil {
		return "Error: " + err.Error()
	}
	if status != 200 {
		return fmt.Sprintf("Error: Brave web search failed: %s (status %d)", string(data), status)
	}
	results, err := parseBraveResults(data)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatSearchResults(results)
}

func parseBraveResults(data []byte) ([]searchResult, error) {
	var resp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var out []searchResult
	for _, r := range resp.Web.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Firecrawl
// ---------------------------------------------------------------------------

func firecrawlKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_firecrawl_key")
}

func executeWebSearchFirecrawl(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := firecrawlKeys(cfg)
	if len(keys) == 0 {
		return "Error: Firecrawl API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: web_search_firecrawl requires a query."
	}
	limit := argIntDefault(args, "limit", 5)
	if limit < 1 {
		limit = 1
	}
	if limit > 20 {
		limit = 20
	}
	payload := map[string]interface{}{
		"query":   query,
		"limit":   limit,
		"sources": []string{"web"},
	}
	if v := strings.TrimSpace(argString(args, "country")); v != "" {
		payload["country"] = v
	}
	if v := strings.TrimSpace(argString(args, "location")); v != "" {
		payload["location"] = v
	}
	results, err := callSearchWithKeys("https://api.firecrawl.dev/v2/search", keys, "Authorization", "Bearer ",
		payload, parseFirecrawlResults)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatSearchResults(results)
}

func parseFirecrawlResults(data []byte) ([]searchResult, error) {
	var resp struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var items []map[string]interface{}
	var dataMap map[string]json.RawMessage
	if err := json.Unmarshal(resp.Data, &dataMap); err == nil {
		if web, ok := dataMap["web"]; ok {
			_ = json.Unmarshal(web, &items)
		}
	} else {
		_ = json.Unmarshal(resp.Data, &items)
	}
	var out []searchResult
	for _, r := range items {
		title, _ := r["title"].(string)
		url, _ := r["url"].(string)
		snippet, _ := r["description"].(string)
		if snippet == "" {
			snippet, _ = r["snippet"].(string)
		}
		if snippet == "" {
			snippet, _ = r["markdown"].(string)
		}
		if url == "" {
			continue
		}
		out = append(out, searchResult{Title: title, URL: url, Snippet: snippet})
	}
	return out, nil
}

func executeFirecrawlExtract(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := firecrawlKeys(cfg)
	if len(keys) == 0 {
		return "Error: Firecrawl API key is not configured in AstrBot."
	}
	url := strings.TrimSpace(argString(args, "url"))
	if url == "" {
		return "Error: firecrawl_extract_web_page requires a url."
	}
	format := argStringDefault(args, "output_format", "markdown")
	if format != "markdown" && format != "html" && format != "rawHtml" && format != "summary" {
		format = "markdown"
	}
	payload := map[string]interface{}{
		"url":             url,
		"formats":         []string{format},
		"onlyMainContent": argBoolDefault(args, "only_main_content", true),
	}
	data, status, err := httpJSON(http.MethodPost, "https://api.firecrawl.dev/v2/scrape",
		map[string]string{"Authorization": "Bearer " + keys[0]}, payload)
	if err != nil {
		return "Error: " + err.Error()
	}
	if status != 200 {
		return fmt.Sprintf("Error: Firecrawl scrape failed: %s (status %d)", string(data), status)
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "Error: " + err.Error()
	}
	if resp.Data == nil {
		return "Error: Firecrawl scrape returned no data."
	}
	content, _ := resp.Data[format].(string)
	resultURL, _ := resp.Data["url"].(string)
	if resultURL == "" {
		resultURL = url
	}
	return fmt.Sprintf("URL: %s\nContent: %s", resultURL, content)
}

// ---------------------------------------------------------------------------
// Baidu AI Search
// ---------------------------------------------------------------------------

func executeWebSearchBaidu(cfg map[string]interface{}, args map[string]interface{}) string {
	apiKey := providerString(cfg, "websearch_baidu_app_builder_key")
	if apiKey == "" {
		return "Error: Baidu AI Search API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: web_search_baidu requires a query."
	}
	if len([]rune(query)) > 72 {
		query = string([]rune(query)[:72])
	}
	topK := argIntDefault(args, "top_k", 10)
	if topK < 1 {
		topK = 1
	}
	if topK > 50 {
		topK = 50
	}
	payload := map[string]interface{}{
		"messages":             []map[string]interface{}{{"role": "user", "content": query}},
		"search_source":        "baidu_search_v2",
		"resource_type_filter": []map[string]interface{}{{"type": "web", "top_k": topK}},
	}
	if v := strings.TrimSpace(argString(args, "search_recency_filter")); v != "" {
		payload["search_recency_filter"] = v
	}
	data, status, err := httpJSON(http.MethodPost, "https://qianfan.baidubce.com/v2/ai_search/web_search",
		map[string]string{
			"Authorization":              "Bearer " + apiKey,
			"X-Appbuilder-Authorization": "Bearer " + apiKey,
		}, payload)
	if err != nil {
		return "Error: " + err.Error()
	}
	if status != 200 {
		return fmt.Sprintf("Error: Baidu web search failed: %s (status %d)", string(data), status)
	}
	var resp struct {
		References []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"references"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "Error: " + err.Error()
	}
	var out []searchResult
	for _, r := range resp.References {
		if r.URL == "" {
			continue
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	if len(out) == 0 {
		return "Error: Baidu AI Search does not return any results."
	}
	return formatSearchResults(out)
}

// ---------------------------------------------------------------------------
// Exa
// ---------------------------------------------------------------------------

func exaKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_exa_key")
}

func executeWebSearchExa(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := exaKeys(cfg)
	if len(keys) == 0 {
		return "Error: Exa API key is not configured in AstrBot."
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: web_search_exa requires a query."
	}
	numResults := argIntDefault(args, "num_results", 10)
	if numResults < 1 {
		numResults = 1
	}
	if numResults > 20 {
		numResults = 20
	}
	payload := map[string]interface{}{
		"query":      query,
		"numResults": numResults,
		"type":       argStringDefault(args, "search_type", "auto"),
		"contents":   map[string]interface{}{"text": map[string]interface{}{"maxCharacters": 500}},
	}
	if v := strings.TrimSpace(argString(args, "category")); v != "" {
		payload["category"] = v
	}
	results, err := callSearchWithKeys("https://api.exa.ai/search", keys, "x-api-key", "",
		payload, parseExaResults)
	if err != nil {
		return "Error: " + err.Error()
	}
	return formatSearchResults(results)
}

func parseExaResults(data []byte) ([]searchResult, error) {
	var resp struct {
		Results []struct {
			Title      string   `json:"title"`
			URL        string   `json:"url"`
			Text       string   `json:"text"`
			Highlights []string `json:"highlights"`
			Summary    string   `json:"summary"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var out []searchResult
	for _, r := range resp.Results {
		if r.URL == "" {
			continue
		}
		snippet := r.Text
		if snippet == "" && len(r.Highlights) > 0 {
			snippet = r.Highlights[0]
		}
		if snippet == "" {
			snippet = r.Summary
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: snippet})
	}
	return out, nil
}

func executeExaGetContents(cfg map[string]interface{}, args map[string]interface{}) string {
	keys := exaKeys(cfg)
	if len(keys) == 0 {
		return "Error: Exa API key is not configured in AstrBot."
	}
	url := strings.TrimSpace(argString(args, "url"))
	if url == "" {
		return "Error: exa_get_contents requires a url."
	}
	maxChars := argIntDefault(args, "max_characters", 3000)
	payload := map[string]interface{}{
		"ids":  []string{url},
		"text": map[string]interface{}{"maxCharacters": maxChars},
	}
	data, status, err := httpJSON(http.MethodPost, "https://api.exa.ai/contents",
		map[string]string{"x-api-key": keys[0]}, payload)
	if err != nil {
		return "Error: " + err.Error()
	}
	if status != 200 {
		return fmt.Sprintf("Error: Exa contents failed: %s (status %d)", string(data), status)
	}
	var resp struct {
		Results []struct {
			URL  string `json:"url"`
			Text string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "Error: " + err.Error()
	}
	if len(resp.Results) == 0 {
		return "Error: Exa contents returned no results."
	}
	r := resp.Results[0]
	return fmt.Sprintf("URL: %s\nContent: %s", orEmpty(r.URL, "No URL"), orEmpty(r.Text, "No content"))
}

// ---------------------------------------------------------------------------
// AnySearch
// ---------------------------------------------------------------------------

func anySearchKeys(cfg map[string]interface{}) []string {
	return providerStringKeys(cfg, "websearch_anysearch_key")
}

// executeWebSearchAnySearch 对齐 Python v4.28.0 #9767 AnySearch：
// AnySearch 支持匿名调用（每日免费额度），key 列表为空时发送一次无
// Authorization 的请求；配置了 key 时按顺序轮询，遇到可重试状态码
// {401,402,403,429} 换下一个 key 重试直至耗尽，其余状态码直接失败。
func executeWebSearchAnySearch(cfg map[string]interface{}, args map[string]interface{}) string {
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "Error: web_search_anysearch requires a query."
	}
	maxResults := argIntDefault(args, "max_results", 10)
	if maxResults < 1 {
		maxResults = 1
	}
	if maxResults > 20 {
		maxResults = 20
	}
	payload := map[string]interface{}{
		"query":       query,
		"max_results": maxResults,
		"format":      "json",
	}
	if v := strings.TrimSpace(argString(args, "tag")); v != "" {
		payload["tag"] = v
	}
	if v := strings.TrimSpace(argString(args, "zone")); v == "cn" || v == "intl" {
		payload["zone"] = v
	}
	if v := strings.TrimSpace(argString(args, "language")); v != "" {
		payload["language"] = v
	}

	results, err := anySearchSearch(cfg, payload)
	if err != nil {
		return "Error: " + err.Error()
	}
	if len(results) == 0 {
		return "Error: AnySearch web search does not return any results."
	}
	return formatSearchResults(results)
}

// anySearchSearch POSTs to the AnySearch /v1/search endpoint with key
// failover; a nil/empty key list results in one anonymous request. 对齐
// Python v4.28.0 #9767：可重试状态码 {401,402,403,429} 换下一个 key 重试
// 至 keys 耗尽后抛最后错误，其余状态码直接失败。
func anySearchSearch(cfg map[string]interface{}, payload interface{}) ([]searchResult, error) {
	keys := anySearchKeys(cfg)
	if len(keys) == 0 {
		// key 列表为空：匿名调用一次，headers 无 Authorization。
		data, status, err := httpJSON(http.MethodPost, "https://api.anysearch.com/v1/search",
			map[string]string{"Content-Type": "application/json"}, payload)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("AnySearch web search failed: %s, status: %d", string(data), status)
		}
		return parseAnySearchResults(data)
	}
	var lastErr error
	for _, key := range keys {
		headers := map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + key,
		}
		data, status, err := httpJSON(http.MethodPost, "https://api.anysearch.com/v1/search", headers, payload)
		if err != nil {
			lastErr = err
			continue
		}
		if status != 200 {
			lastErr = fmt.Errorf("AnySearch web search failed: %s, status: %d", string(data), status)
			if !anySearchRetryableStatus(status) {
				return nil, lastErr
			}
			continue
		}
		return parseAnySearchResults(data)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("AnySearch web search failed with all configured keys.")
}

// anySearchRetryableStatus 对齐 Python 的 _ANYSEARCH_RETRYABLE_HTTP_STATUSES：
// 401 未授权 / 402 余额不足 / 403 禁用 / 429 限流，均可换 key 重试。
func anySearchRetryableStatus(status int) bool {
	switch status {
	case 401, 402, 403, 429:
		return true
	}
	return false
}

func parseAnySearchResults(data []byte) ([]searchResult, error) {
	var resp struct {
		Data struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
				Content string `json:"content"`
			} `json:"results"`
		} `json:"data"`
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	// results 在 data.results，兼容顶层 results。
	items := resp.Data.Results
	if len(items) == 0 {
		items = resp.Results
	}
	var out []searchResult
	for _, r := range items {
		if r.URL == "" {
			continue
		}
		snippet := r.Snippet
		if snippet == "" {
			snippet = r.Content
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: snippet})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

func formatSearchResults(results []searchResult) string {
	if len(results) == 0 {
		return "No results."
	}
	var sb strings.Builder
	sb.WriteString("[搜索结果]\n")
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   链接: %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet))
	}
	return sb.String()
}

// callSearchWithKeys POSTs a payload with each key (Bearer or plain header
// prefix) until one succeeds, then parses via the provided parser.
func callSearchWithKeys(url string, keys []string, headerKey, headerPrefix string, payload interface{}, parse func([]byte) ([]searchResult, error)) ([]searchResult, error) {
	var lastErr error
	for _, key := range keys {
		headers := map[string]string{}
		if headerPrefix != "" {
			headers[headerKey] = headerPrefix + key
		} else {
			headers[headerKey] = key
		}
		data, status, err := httpJSON(http.MethodPost, url, headers, payload)
		if err != nil {
			lastErr = err
			continue
		}
		if status != 200 {
			lastErr = fmt.Errorf("%s (status %d)", string(data), status)
			continue
		}
		results, err := parse(data)
		if err != nil {
			lastErr = err
			continue
		}
		return results, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("failed with all configured keys")
}

// argStringDefault reads a string arg with a default.
func argStringDefault(args map[string]interface{}, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}

// argIntDefault reads an int arg with a default.
func argIntDefault(args map[string]interface{}, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}

// argBoolDefault reads a bool arg with a default.
func argBoolDefault(args map[string]interface{}, key string, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}

func orEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
