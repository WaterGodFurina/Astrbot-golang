package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/log"
)

// marketCacheTTL controls how long a fetched registry snapshot is served from
// memory before it is refetched.
const marketCacheTTL = 5 * time.Minute

// fetchPluginMarket returns the plugin market registry data for the given
// registry URL (defaulting to defaultPluginMarketURL), honoring an in-memory
// cache unless forceRefresh is set. On fetch failure it falls back to the
// cached snapshot; with no cache it returns an error.
func (s *Server) fetchPluginMarket(registryURL string, forceRefresh bool) (interface{}, error) {
	url := strings.TrimSpace(registryURL)
	if url == "" {
		url = defaultPluginMarketURL
	}

	s.marketMu.Lock()
	defer s.marketMu.Unlock()

	if !forceRefresh {
		if entry, ok := s.marketCache[url]; ok && time.Since(entry.fetchedAt) < marketCacheTTL {
			return entry.data, nil
		}
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			data, decErr := decodeMarketBody(resp.Body)
			if decErr == nil {
				s.marketCache[url] = &marketCacheEntry{data: data, fetchedAt: time.Now()}
				return data, nil
			}
			log.GetDefault().Info("plugin market decode failed: %v", decErr)
		} else {
			log.GetDefault().Info("plugin market fetch failed with status %d", resp.StatusCode)
		}
	} else {
		log.GetDefault().Info("plugin market fetch failed: %v", err)
	}

	if entry, ok := s.marketCache[url]; ok {
		return entry.data, nil
	}
	return nil, err
}

// decodeMarketBody reads and parses a registry JSON payload. It tolerates both
// a plain JSON object and a {data, timestamp} cache wrapper.
func decodeMarketBody(r io.Reader) (interface{}, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if wrapped, ok := raw["data"]; ok {
		return wrapped, nil
	}
	return raw, nil
}
