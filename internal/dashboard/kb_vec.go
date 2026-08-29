package dashboard

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/knowledgebase"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
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
	if err != nil {
		return err
	}
	if len(chunks) > 0 {
		if _, dim, derr := s.kbEmbeddingProvider(kbID); derr != nil {
			logger.I18nWarn("删除文档 %s 的向量失败（嵌入模型不可用: %v），向量库可能残留孤儿向量", docID, derr)
		} else {
			lock := kbVecLock(kbID)
			lock.Lock()
			vdb, oerr := knowledgebase.OpenVecDB(s.kbVecDir(kbID), kbID, dim)
			if oerr != nil {
				lock.Unlock()
				logger.I18nWarn("打开向量库失败，文档 %s 的向量无法删除: %v", docID, oerr)
				return fmt.Errorf("删除向量索引失败: %w", oerr)
			}
			for _, c := range chunks {
				if err := vdb.Delete(c.ChunkID); err != nil {
					logger.I18nWarn("删除向量 %s 失败: %v", c.ChunkID, err)
				}
			}
			if err := vdb.Vacuum(); err != nil {
				logger.I18nWarn("向量库压缩失败: %v", err)
			}
			if err := vdb.Close(); err != nil {
				logger.I18nWarn("关闭向量库失败: %v", err)
			}
			lock.Unlock()
		}
	}
	return s.database.DeleteKBDoc(kbID, docID)
}

// kbNamesForSession resolves the knowledge bases for a session: the session
// kb_config rule (kb_ids) wins; an explicit empty kb_ids disables KB. Falls
// back to the global kb_names config. Returns (kbIDs, topK, enabled).
func (s *Server) kbNamesForSession(umo string) ([]string, int, bool) {
	if cm, ok := s.conversationMgr.(interface {
		GetSessionRules(umo string) map[string]interface{}
	}); ok {
		if rules := cm.GetSessionRules(umo); rules != nil {
			if kc, ok := rules["kb_config"].(map[string]interface{}); ok {
				rawIDs, has := kc["kb_ids"].([]interface{})
				if has && len(rawIDs) == 0 {
					return nil, 0, false
				}
				var ids []string
				for _, id := range rawIDs {
					if s, ok := id.(string); ok && s != "" {
						ids = append(ids, s)
					}
				}
				if len(ids) > 0 || has {
					topK := 5
					if v, ok := kc["top_k"].(float64); ok && v > 0 {
						topK = int(v)
					}
					return ids, topK, true
				}
			}
		}
	}
	cfg := s.getConfigData("default")
	raw, _ := cfg["kb_names"].([]interface{})
	if len(raw) == 0 {
		return nil, 0, false
	}
	var ids []string
	for _, n := range raw {
		if str, ok := n.(string); ok && str != "" {
			ids = append(ids, str)
		}
	}
	topK := 5
	if v, ok := cfg["kb_final_top_k"].(float64); ok && v > 0 {
		topK = int(v)
	}
	return ids, topK, len(ids) > 0
}

// RetrieveKBContext resolves KB context text for a session+prompt, respecting
// the session kb_config rule. Returns formatted reference text ("" when no KB
// applies). Exported for the pipeline KB retriever.
func (s *Server) RetrieveKBContext(umo, query string) (string, error) {
	kbIDs, topK, ok := s.kbNamesForSession(umo)
	if !ok || len(kbIDs) == 0 || s.database == nil {
		return "", nil
	}
	// kbNamesForSession may return names (global kb_names) or ids (session
	// kb_config.kb_ids); resolve both to kb_id before vector retrieval.
	lookup := map[string]string{}
	if rows, err := s.database.ListKBs(); err == nil {
		for i := range rows {
			lookup[rows[i].KBID] = rows[i].KBID
			if rows[i].KBName != "" {
				lookup[rows[i].KBName] = rows[i].KBID
			}
		}
	}
	var resolved []string
	for _, ref := range kbIDs {
		if id, ok := lookup[ref]; ok {
			resolved = append(resolved, id)
		} else {
			logger.I18nWarn("知识库 %q 不存在，已跳过", ref)
		}
	}
	if len(resolved) == 0 {
		return "", nil
	}
	var sb strings.Builder
	for _, kbID := range resolved {
		results, err := s.kbRetrieve(kbID, query, topK)
		if err != nil {
			logger.I18nWarn("知识库 %s 检索失败: %v", kbID, err)
			continue
		}
		for i, hit := range results {
			sb.WriteString(fmt.Sprintf("【知识 %d】\n%s\n", i+1, hit.Content))
		}
	}
	return sb.String(), nil
}

// RetrieveKBByNames runs a vector retrieval across the given knowledge bases
// (names or IDs; empty = all KBs, aligned with Python kb_mgr.retrieve 的
// "kb_names 空 = 全部启用 KB" 语义) and returns the concatenated context text
// plus per-hit result maps (字段对齐 Python 本体 results dict：chunk_id/
// doc_id/kb_id/kb_name/doc_name/chunk_index/content/score/char_count)。
// topMFinal <=0 用默认 5；topKFusion 保留入参（当前向量后端为单路召回，
// 融合条数与最终条数一致）。Exported for the plugin HostService KBRetrieve
// hook（经 lifecycle 闭包注入 plugin.HostServiceExtras.KBRetriever）。
func (s *Server) RetrieveKBByNames(query string, kbNames []string, topKFusion, topMFinal int) (string, []map[string]any, error) {
	if s.database == nil {
		return "", nil, fmt.Errorf("数据库不可用")
	}
	rows, err := s.database.ListKBs()
	if err != nil {
		return "", nil, err
	}
	lookup := map[string]db.KBRow{}
	for _, row := range rows {
		lookup[row.KBID] = row
		if row.KBName != "" {
			lookup[row.KBName] = row
		}
	}
	var resolved []string
	if len(kbNames) == 0 {
		for _, row := range rows {
			resolved = append(resolved, row.KBID)
		}
	} else {
		for _, ref := range kbNames {
			if row, ok := lookup[ref]; ok {
				resolved = append(resolved, row.KBID)
			} else {
				logger.I18nWarn("知识库 %q 不存在，已跳过", ref)
			}
		}
	}
	if len(resolved) == 0 {
		return "", []map[string]any{}, nil
	}
	topK := topMFinal
	if topK <= 0 {
		topK = 5
	}

	var sb strings.Builder
	results := []map[string]any{}
	idx := 0
	for _, kbID := range resolved {
		hits, err := s.kbRetrieve(kbID, query, topK)
		if err != nil {
			logger.I18nWarn("知识库 %s 检索失败: %v", kbID, err)
			continue
		}
		if len(hits) == 0 {
			continue
		}
		kbName := lookup[kbID].KBName
		// 按 docID 聚合后一次性查 SQLite 分块表，回填 chunk_index
		//（向量索引元数据未随 SearchResult 返回，SQLite 是 chunk 元数据
		// source of truth）。
		byDoc := map[string][]knowledgebase.SearchResult{}
		var docOrder []string
		for _, hit := range hits {
			if _, seen := byDoc[hit.DocID]; !seen {
				docOrder = append(docOrder, hit.DocID)
			}
			byDoc[hit.DocID] = append(byDoc[hit.DocID], hit)
		}
		chunkIdx := map[string]int{}
		for _, docID := range docOrder {
			if chunks, err := s.database.ListKBChunks(kbID, docID); err == nil {
				for _, c := range chunks {
					chunkIdx[c.ChunkID] = c.ChunkIdx
				}
			}
		}
		for _, hit := range hits {
			idx++
			sb.WriteString(fmt.Sprintf("【知识 %d】\n%s\n", idx, hit.Content))
			content := hit.Content
			results = append(results, map[string]any{
				"chunk_id":    hit.ChunkID,
				"doc_id":      hit.DocID,
				"kb_id":       kbID,
				"kb_name":     kbName,
				"doc_name":    hit.DocName,
				"chunk_index": chunkIdx[hit.ChunkID],
				"content":     content,
				"score":       float64(hit.Score),
				"char_count":  len([]rune(content)),
			})
		}
	}
	return sb.String(), results, nil
}
