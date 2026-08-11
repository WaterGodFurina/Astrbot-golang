package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/db"
	"github.com/AstrBotDevs/AstrBot/internal/knowledgebase"
	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

// kbVecMu serializes vector writes per knowledge base (nanovec is not safe for
// concurrent writers on the same file, and chunking/embedding is CPU/network
// bound). Keyed by kb_id.
var kbVecMu sync.Map // map[string]*sync.Mutex

func kbVecLock(kbID string) *sync.Mutex {
	m, _ := kbVecMu.LoadOrStore(kbID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// kbVecDir returns the vector index directory for a knowledge base.
func (s *Server) kbVecDir(kbID string) string {
	return filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID))
}

// kbVecPath returns the nanovec base path (nanovec appends .store/.idx).
func (s *Server) kbVecPath(kbID string) string {
	return filepath.Join(s.kbVecDir(kbID), kbID)
}

// kbEmbeddingProvider builds an embedding provider instance from the KB's
// configured embedding provider id. It mirrors the pipeline's lazy provider
// creation: resolve the provider config, merge its source, then CreateProvider
// with the `type` field.
func (s *Server) kbEmbeddingProvider(kbID string) (provider.EmbeddingProvider, int, error) {
	row, err := s.database.GetKB(kbID)
	if err != nil {
		return nil, 0, err
	}
	embedID := row.EmbeddingProviderID
	pc := s.getProviderByID(embedID)
	if len(pc) == 0 {
		return nil, 0, fmt.Errorf("嵌入模型不存在: %s", embedID)
	}
	pType, _ := pc["type"].(string)
	if pType == "" {
		pType, _ = pc["provider_type"].(string)
	}
	if pType == "" {
		return nil, 0, fmt.Errorf("嵌入模型缺少 type: %s", embedID)
	}
	merged := s.mergeProviderSource(pc)
	inst, err := provider.CreateProvider(pType, merged, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("初始化嵌入模型失败: %w", err)
	}
	ep, ok := inst.(provider.EmbeddingProvider)
	if !ok {
		return nil, 0, fmt.Errorf("提供商 %s 不是嵌入模型", pType)
	}
	dim := 0
	if v, ok := merged["embedding_dimensions"]; ok {
		switch n := v.(type) {
		case float64:
			dim = int(n)
		case int:
			dim = n
		}
	}
	return ep, dim, nil
}

// embedChunk generates the embedding for one chunk.
func (s *Server) embedChunk(ep provider.EmbeddingProvider, text string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return ep.GetEmbedding(ctx, text)
}

// indexKBFile chunks a document, embeds each chunk, writes to SQLite first
// (list source of truth) then nanovec (vector index). A nanovec failure does
// not roll back SQLite — the index can be rebuilt from SQLite later.
func (s *Server) indexKBFile(kbID, docID, docName string, content []byte, chunkSize, chunkOverlap int) (int, error) {
	if s.database == nil {
		return 0, fmt.Errorf("数据库不可用")
	}
	ep, dim, err := s.kbEmbeddingProvider(kbID)
	if err != nil {
		return 0, err
	}

	// 幂等性：同名文档重复上传、或上次嵌入失败留下残留分块时，
	// 先清掉该 docID 的旧分块记录，避免 UNIQUE 冲突与"半成品"残留。
	if err := s.database.DeleteKBChunks(kbID, docID); err != nil {
		return 0, fmt.Errorf("清理旧分块记录失败: %w", err)
	}

	chunks := knowledgebase.ChunkText(string(content), chunkSize, chunkOverlap)
	if len(chunks) == 0 {
		return 0, fmt.Errorf("文档内容为空")
	}
	if dim <= 0 {
		// Try to detect dimension from the first embedding.
		if v, err := s.embedChunk(ep, chunks[0]); err == nil {
			dim = len(v)
		} else {
			return 0, fmt.Errorf("无法确定向量维度: %w", err)
		}
	}

	// Phase 1: persist chunk records to SQLite (list source of truth).
	var vecChunks []knowledgebase.VectorChunk
	for i, c := range chunks {
		chunkID := fmt.Sprintf("chunk_%s_%d", docID, i)
		if err := s.database.InsertKBChunk(db.KBChunk{
			ChunkID:  chunkID,
			KBID:     kbID,
			DocID:    docID,
			DocName:  docName,
			Content:  c,
			ChunkIdx: i,
		}); err != nil {
			return 0, fmt.Errorf("写入分块记录失败: %w", err)
		}
		vecChunks = append(vecChunks, knowledgebase.VectorChunk{
			ChunkID:  chunkID,
			DocID:    docID,
			DocName:  docName,
			KBID:     kbID,
			ChunkIdx: i,
			Content:  c,
		})
	}

	// Phase 2: embed all chunks and write to nanovec.
	// 嵌入/写入任一环节失败时，defer 回滚已写入的 SQLite 分块，
	// 避免留下"有分块无向量"的半成品（下次可重新上传索引）。
	vecs := make([][]float32, 0, len(vecChunks))
	succeeded := false
	defer func() {
		if !succeeded {
			_ = s.database.DeleteKBChunks(kbID, docID)
		}
	}()
	for _, c := range vecChunks {
		v, err := s.embedChunk(ep, c.Content)
		if err != nil {
			return len(vecChunks), fmt.Errorf("嵌入第 %d 块失败: %w（分块已回滚，可稍后重试）", c.ChunkIdx, err)
		}
		vecs = append(vecs, v)
	}

	lock := kbVecLock(kbID)
	lock.Lock()
	defer lock.Unlock()
	vdb, err := knowledgebase.OpenVecDB(s.kbVecDir(kbID), kbID, dim)
	if err != nil {
		return len(vecChunks), fmt.Errorf("打开向量索引失败: %w", err)
	}
	defer vdb.Close()
	if err := vdb.InsertBatch(vecChunks, vecs); err != nil {
		return len(vecChunks), fmt.Errorf("写入向量索引失败: %w", err)
	}
	succeeded = true
	return len(vecChunks), nil
}

// syncKBVecDB reconciles the nanovec index with the SQLite chunk records
// (source of truth). If they disagree, it deletes the .idx file so nanovec's
// Open-time self-healing rebuilds from its own bbolt store, then re-inserts
// any chunks missing from the vector index.
func (s *Server) syncKBVecDB(kbID string) error {
	if s.database == nil {
		return nil
	}
	sqliteChunks, err := s.database.ListKBChunks(kbID, "")
	if err != nil {
		return err
	}
	if len(sqliteChunks) == 0 {
		return nil
	}
	ep, dim, err := s.kbEmbeddingProvider(kbID)
	if err != nil {
		return err
	}

	lock := kbVecLock(kbID)
	lock.Lock()
	defer lock.Unlock()

	vdb, err := knowledgebase.OpenVecDB(s.kbVecDir(kbID), kbID, dim)
	if err != nil {
		// Index file is corrupt; drop it and let nanovec rebuild from bbolt.
		_ = os.Remove(s.kbVecPath(kbID) + ".idx")
		return nil
	}
	defer vdb.Close()

	// nanovec's Delete + Open self-healing keeps the .store (bbolt) in sync with
	// the .idx, so a version mismatch auto-rebuilds. For SQLite ↔ nanovec drift,
	// we re-insert chunks that are missing from the index by re-embedding.
	count, err := vdb.Count()
	if err != nil || count < len(sqliteChunks) {
		// Index is missing chunks (e.g. nanovec insert failed after SQLite
		// succeeded). Re-embed from SQLite records.
		var missing []knowledgebase.VectorChunk
		var vecs [][]float32
		for _, c := range sqliteChunks {
			v, err := s.embedChunk(ep, c.Content)
			if err != nil {
				continue
			}
			missing = append(missing, knowledgebase.VectorChunk{
				ChunkID:  c.ChunkID,
				DocID:    c.DocID,
				DocName:  c.DocName,
				KBID:     c.KBID,
				ChunkIdx: c.ChunkIdx,
				Content:  c.Content,
			})
			vecs = append(vecs, v)
		}
		if len(missing) > 0 {
			_ = vdb.InsertBatch(missing, vecs)
		}
	}
	return nil
}

// kbRetrieve runs a vector similarity search against a knowledge base.
func (s *Server) kbRetrieve(kbID, query string, topK int) ([]knowledgebase.SearchResult, error) {
	if s.database == nil {
		return nil, fmt.Errorf("数据库不可用")
	}
	ep, dim, err := s.kbEmbeddingProvider(kbID)
	if err != nil {
		return nil, err
	}
	qvec, err := s.embedChunk(ep, query)
	if err != nil {
		return nil, fmt.Errorf("查询嵌入失败: %w", err)
	}
	if dim <= 0 {
		dim = len(qvec)
	}

	lock := kbVecLock(kbID)
	lock.Lock()
	defer lock.Unlock()

	vdb, err := knowledgebase.OpenVecDB(s.kbVecDir(kbID), kbID, dim)
	if err != nil {
		return nil, err
	}
	defer vdb.Close()
	return vdb.Search(qvec, topK, "")
}

// kbDeleteDoc removes a document: delete its nanovec vectors first (soft
// delete), then its SQLite chunk rows (list source of truth).
func (s *Server) kbDeleteDoc(kbID, docID string) error {
	if s.database == nil {
		return nil
	}
	chunks, err := s.database.ListKBChunks(kbID, docID)
	if err == nil && len(chunks) > 0 {
		if _, dim, derr := s.kbEmbeddingProvider(kbID); derr == nil {
			lock := kbVecLock(kbID)
			lock.Lock()
			if vdb, oerr := knowledgebase.OpenVecDB(s.kbVecDir(kbID), kbID, dim); oerr == nil {
				for _, c := range chunks {
					_ = vdb.Delete(c.ChunkID)
				}
				_ = vdb.Vacuum()
				_ = vdb.Close()
			}
			lock.Unlock()
		}
	}
	return s.database.DeleteKBDoc(kbID, docID)
}
