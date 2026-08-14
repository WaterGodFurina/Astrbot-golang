package knowledgebase

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hungpdn/nanovec"
	"github.com/hungpdn/nanovec/pkg/types"
)

// VectorChunk is a single text chunk with its embedding metadata.
type VectorChunk struct {
	DocID    string `json:"doc_id"`
	ChunkID  string `json:"chunk_id"`
	Content  string `json:"content"`
	DocName  string `json:"doc_name"`
	KBID     string `json:"kb_id"`
	ChunkIdx int    `json:"chunk_index"`
}

// VecDB wraps nanovec for a single knowledge base's vector index. It stores
// each text chunk as a document: the vector is the chunk embedding, and the
// metadata carries doc_id/doc_name/kb_id/chunk_index so retrieval results map
// back to documents. Chunk content itself is persisted separately (SQLite) for
// listing and rendering; nanovec only keeps vectors + small metadata.
type VecDB struct {
	db   *nanovec.DB
	path string
}

// OpenVecDB opens (or creates) the vector index for a knowledge base.
func OpenVecDB(dir, kbID string, dimension int) (*VecDB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, kbID+".vec.db")
	cfg := &nanovec.Config{
		Dimension: dimension,
		IndexType: nanovec.IndexTypeFlat, // exact search, matches FAISS IndexFlatL2
		ReadOnly:  false,
	}
	db, err := nanovec.Open(path, cfg)
	if err != nil {
		return nil, err
	}
	return &VecDB{db: db, path: path}, nil
}

// Close closes the underlying index.
func (v *VecDB) Close() error {
	if v != nil && v.db != nil {
		return v.db.Close()
	}
	return nil
}

// InsertBatch adds chunks in one transaction (much faster than Insert for many
// chunks, and matches the "Batch Insert" recommendation in nanovec docs). The
// full chunk content is stored in nanovec's metadata so retrieval results carry
// the text directly; a separate SQLite chunk index (dashboard layer) keeps an
// enumerable list for the /chunks endpoint (nanovec has no scan API).
func (v *VecDB) InsertBatch(chunks []VectorChunk, vecs [][]float32) error {
	ids := make([]string, len(chunks))
	metas := make([]map[string]any, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ChunkID
		metas[i] = map[string]any{
			"doc_id":      c.DocID,
			"doc_name":    c.DocName,
			"kb_id":       c.KBID,
			"chunk_index": c.ChunkIdx,
			"content":     c.Content,
		}
	}
	return v.db.InsertBatch(ids, vecs, metas)
}

// SearchResult is a retrieval hit with its similarity score and metadata.
type SearchResult struct {
	ChunkID string
	DocID   string
	DocName string
	Content string
	Score   float32
}

// Search retrieves the top-k most similar chunks, optionally filtered by
// doc_id. nanovec uses cosine similarity, so higher Score = more relevant.
func (v *VecDB) Search(queryVec []float32, k int, docID string) ([]SearchResult, error) {
	var filter types.FilterFunc
	if docID != "" {
		filter = func(m map[string]any) bool {
			return m["doc_id"] == docID
		}
	}
	results, err := v.db.Search(queryVec, k, filter)
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		out = append(out, SearchResult{
			ChunkID: r.ID,
			Score:   r.Score,
			DocID:   strAny(r.Metadata["doc_id"]),
			DocName: strAny(r.Metadata["doc_name"]),
			Content: strAny(r.Metadata["content"]),
		})
	}
	return out, nil
}

// Count returns the total number of stored vectors (chunks).
func (v *VecDB) Count() (int, error) {
	return v.db.Count()
}

// Delete removes a chunk (soft delete; call Vacuum to purge).
func (v *VecDB) Delete(chunkID string) error {
	return v.db.Delete(chunkID)
}

// Vacuum rebuilds the index from bbolt storage to purge soft-deleted vectors.
func (v *VecDB) Vacuum() error {
	return v.db.Vacuum()
}

// CloseDB closes and forgets the underlying handle (kept for API symmetry).
func (v *VecDB) CloseDB() {
	_ = v.Close()
}

// strAny converts an any metadata value to string.
func strAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ChunkText splits text into overlapping chunks: a chunk is up to chunkSize
// runes, stepping by (chunkSize - chunkOverlap), broken at newlines when
// possible. Returns empty for empty input.
func ChunkText(text string, chunkSize, chunkOverlap int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if chunkOverlap < 0 {
		chunkOverlap = 0
	}
	if chunkOverlap >= chunkSize {
		chunkOverlap = chunkSize - 1
	}
	runes := []rune(text)
	if len(runes) <= chunkSize {
		return []string{text}
	}
	var chunks []string
	step := chunkSize - chunkOverlap
	// nextStart 记录实际消费位置：按换行截断时，下一 chunk 起点同步前移到截断
	// 点，避免 [截断点, start+step) 区间不属于任何 chunk 造成文本静默丢失。
	nextStart := 0
	for start := 0; start < len(runes); {
		end := start + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		content := string(runes[start:end])
		consumed := end
		if nl := strings.LastIndex(content, "\n"); nl > chunkSize/2 {
			content = content[:nl]
			consumed = start + nl
		}
		content = strings.TrimSpace(content)
		if content != "" {
			chunks = append(chunks, content)
		}
		if end == len(runes) {
			break
		}
		// 下一 chunk 起点为 max(start+step, 实际消费位置) 中的较小者：若换行
		// 截断把有效结尾前移（nl < step），则从截断点续上，保证无缝隙。
		nextStart = start + step
		if consumed < nextStart {
			nextStart = consumed
		}
		if nextStart <= start {
			nextStart = start + step
		}
		start = nextStart
	}
	return chunks
}
