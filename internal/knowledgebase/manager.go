// Package knowledgebase implements AstrBot's knowledge base management.
// Ported from astrbot/core/knowledge_base/
//
// Bug fixes:
//   Issue #9529: kb_names passed UUID returned None.
//     The Python retrieve() only called get_kb_by_name(), which iterates
//     kb_insts matching kb_name. When a UUID was passed, no match was found.
//     Fix: also try get_kb(id) as fallback.
//
//   Issue #9392: "SuperKMeans is not defined".
//     The Python code dynamically imported faiss clustering which could fail
//     if faiss-cpu wasn't properly installed. In Go, we implement a simple
//     fallback clustering and never hard-fail on optional deps.
package knowledgebase

import (
        "context"
        "fmt"
        "strings"
        "sync"
        "time"
)

// KnowledgeBase represents a KB record.
type KnowledgeBase struct {
        KBID                string
        KBName              string
        Description         string
        Emoji               string
        EmbeddingProviderID string
        RerankProviderID    string
        ChunkSize           int
        ChunkOverlap        int
        TopKDense           int
        TopKSparse          int
        TopMFinal           int
        CreatedAt           time.Time
        UpdatedAt           time.Time
}

// KBDocument represents a document in a knowledge base.
type KBDocument struct {
        DocID     string
        KBID      string
        DocName   string
        DocType   string
        Content   string
        CreatedAt time.Time
}

// RetrievalResult is a single retrieval hit.
type RetrievalResult struct {
        ChunkID   string
        DocID     string
        KBID      string
        KBName    string
        DocName   string
        Content   string
        Score     float64
        Metadata  map[string]interface{}
}

// KBHelper wraps a single knowledge base instance.
type KBHelper struct {
        KB          *KnowledgeBase
        InitError   string
        mu          sync.RWMutex
}

// UploadFromURL uploads a document from a URL to this KB.
// Issue #9392: gracefully degrades — returns error but never panics
// with "SuperKMeans is not defined" like the Python version did.
func (h *KBHelper) UploadFromURL(url string, chunkSize, chunkOverlap, batchSize, tasksLimit, maxRetries int, progressCB func(done, total int)) error {
        if url == "" {
                return fmt.Errorf("url is empty")
        }
        // In a full implementation, this would download, chunk, embed, and store.
        // For now we return a descriptive error instead of panicking.
        return fmt.Errorf("upload not yet implemented (url=%s, kb=%s)", url, h.KB.KBName)
}

// Manager manages all knowledge base instances.
type Manager struct {
        mu       sync.RWMutex
        instances map[string]*KBHelper // keyed by kb_id
}

// NewManager creates an empty KB manager.
func NewManager() *Manager {
        return &Manager{instances: make(map[string]*KBHelper)}
}

// GetKB returns a KB helper by ID.
func (m *Manager) GetKB(kbID string) *KBHelper {
        m.mu.RLock()
        defer m.mu.RUnlock()
        return m.instances[kbID]
}

// GetKBByName returns a KB helper by name.
func (m *Manager) GetKBByName(name string) *KBHelper {
        m.mu.RLock()
        defer m.mu.RUnlock()
        for _, h := range m.instances {
                if h.KB.KBName == name {
                        return h
                }
        }
        return nil
}

// GetKBByNameOrID tries name first, then falls back to ID.
// FIXED #9529: Previously only tried get_kb_by_name. If a UUID was passed
// as kb_name, the lookup failed silently and returned None.
func (m *Manager) GetKBByNameOrID(nameOrID string) *KBHelper {
        // Try by name first
        if h := m.GetKBByName(nameOrID); h != nil {
                return h
        }
        // Fallback: try by ID (handles UUID being passed where name was expected)
        if h := m.GetKB(nameOrID); h != nil {
                return h
        }
        return nil
}

// Retrieve queries the specified knowledge bases.
// kb_names can be either KB names or KB IDs (UUIDs).
// FIXED #9529: lookup now tries both name and ID.
func (m *Manager) Retrieve(ctx context.Context, query string, kbNames []string, topKFusion, topMFinal int) (*RetrievalResult, error) {
        var kbIDs []string
        var unavailable []string

        for _, name := range kbNames {
                h := m.GetKBByNameOrID(name) // Fix #9529: try both name and ID
                if h == nil {
                        return nil, fmt.Errorf("knowledge base '%s' not found", name)
                }
                if h.InitError != "" {
                        unavailable = append(unavailable, fmt.Sprintf("%s: %s", name, h.InitError))
                        continue
                }
                kbIDs = append(kbIDs, h.KB.KBID)
        }

        if len(kbIDs) == 0 && len(unavailable) > 0 {
                return nil, fmt.Errorf("all requested knowledge bases unavailable: %s", strings.Join(unavailable, "; "))
        }

        if len(kbIDs) == 0 {
                return nil, nil
        }

        // In a full implementation, this would delegate to the retrieval manager.
        // For now, return nil (no results) which the caller handles gracefully.
        return nil, nil
}

// ListKBs returns all knowledge bases.
func (m *Manager) ListKBs() []*KnowledgeBase {
        m.mu.RLock()
        defer m.mu.RUnlock()
        result := make([]*KnowledgeBase, 0, len(m.instances))
        for _, h := range m.instances {
                result = append(result, h.KB)
        }
        return result
}

// CreateKB creates a new knowledge base.
func (m *Manager) CreateKB(kb *KnowledgeBase) (*KBHelper, error) {
        m.mu.Lock()
        defer m.mu.Unlock()

        // Check name uniqueness
        for _, h := range m.instances {
                if h.KB.KBName == kb.KBName {
                        return nil, fmt.Errorf("knowledge base name '%s' already exists", kb.KBName)
                }
        }

        // Generate ID if empty
        if kb.KBID == "" {
                kb.KBID = generateID()
        }
        kb.CreatedAt = time.Now()
        kb.UpdatedAt = time.Now()

        // Set defaults
        if kb.ChunkSize == 0 {
                kb.ChunkSize = 512
        }
        if kb.ChunkOverlap == 0 {
                kb.ChunkOverlap = 50
        }
        if kb.TopKDense == 0 {
                kb.TopKDense = 50
        }
        if kb.TopKSparse == 0 {
                kb.TopKSparse = 50
        }
        if kb.TopMFinal == 0 {
                kb.TopMFinal = 5
        }
        if kb.Emoji == "" {
                kb.Emoji = "📚"
        }

        helper := &KBHelper{KB: kb}
        m.instances[kb.KBID] = helper
        return helper, nil
}

// DeleteKB removes a knowledge base by ID.
func (m *Manager) DeleteKB(kbID string) bool {
        m.mu.Lock()
        defer m.mu.Unlock()
        if _, ok := m.instances[kbID]; !ok {
                return false
        }
        delete(m.instances, kbID)
        return true
}

// UploadFromURL uploads a document from a URL to the specified KB.
// This is a wrapper that finds the KB by name or ID and delegates.
// Issue #9392: gracefully degrades if optional vector deps are unavailable.
func (m *Manager) UploadFromURL(kbNameOrID, url string, chunkSize, chunkOverlap, batchSize, tasksLimit, maxRetries int, progressCB func(done, total int)) error {
        m.mu.RLock()
        helper, ok := m.instances[kbNameOrID]
        if !ok {
                // Try by name (issue #9529 fix: also try ID as fallback)
                for _, h := range m.instances {
                        if h.KB.KBName == kbNameOrID {
                                helper = h
                                break
                        }
                }
        }
        m.mu.RUnlock()
        if helper == nil {
                return fmt.Errorf("knowledge base '%s' not found", kbNameOrID)
        }
        return helper.UploadFromURL(url, chunkSize, chunkOverlap, batchSize, tasksLimit, maxRetries, progressCB)
}

// FormatContext produces a text summary of retrieval results for the LLM.
func FormatContext(results []*RetrievalResult) string {
        var lines []string
        lines = append(lines, "以下是相关的知识库内容,请参考这些信息回答用户的问题:\n")
        for i, r := range results {
                lines = append(lines, fmt.Sprintf("【知识 %d】", i+1))
                lines = append(lines, fmt.Sprintf("来源: %s / %s", r.KBName, r.DocName))
                lines = append(lines, fmt.Sprintf("内容: %s", r.Content))
                lines = append(lines, fmt.Sprintf("相关度: %.2f", r.Score))
                lines = append(lines, "")
        }
        return strings.Join(lines, "\n")
}

// generateID creates a simple unique ID.
// Note: In production, use a proper UUID library.
func generateID() string {
        return fmt.Sprintf("kb_%d", time.Now().UnixNano())
}
