// Package dashboard - persona persistence.
// Stores personas and folders as JSON under the data directory.
package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// personaData is the persisted persona/folder store.
type personaData struct {
	Personas []map[string]interface{} `json:"personas"`
	Folders  []map[string]interface{} `json:"folders"`
}

type personaStore struct {
	mu   sync.Mutex
	path string
	data *personaData
}

func newPersonaStore(dataDir string) *personaStore {
	ps := &personaStore{
		path: filepath.Join(dataDir, "personas.json"),
		data: &personaData{Personas: []map[string]interface{}{}, Folders: []map[string]interface{}{}},
	}
	ps.load()
	return ps
}

func (ps *personaStore) load() {
	data, err := os.ReadFile(ps.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, ps.data)
	if ps.data.Personas == nil {
		ps.data.Personas = []map[string]interface{}{}
	}
	if ps.data.Folders == nil {
		ps.data.Folders = []map[string]interface{}{}
	}
}

func (ps *personaStore) save() error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	os.MkdirAll(filepath.Dir(ps.path), 0755)
	data, err := json.MarshalIndent(ps.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ps.path, data, 0644)
}

func nowStr() string {
	return time.Now().Format("2006-01-02T15:04:05.000Z")
}

func (ps *personaStore) listPersonas(folderID *string) []map[string]interface{} {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	result := make([]map[string]interface{}, 0, len(ps.data.Personas))
	for _, p := range ps.data.Personas {
		if folderID != nil {
			fid, _ := p["folder_id"].(string)
			if *folderID == "" {
				if fid != "" {
					continue
				}
			} else if fid != *folderID {
				continue
			}
		}
		result = append(result, p)
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, _ := result[i]["sort_order"].(float64)
		b, _ := result[j]["sort_order"].(float64)
		return a < b
	})
	return result
}

func (ps *personaStore) getPersona(id string) map[string]interface{} {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.data.Personas {
		if pid, _ := p["persona_id"].(string); pid == id {
			return p
		}
	}
	return nil
}

func (ps *personaStore) upsertPersona(p map[string]interface{}) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	id, _ := p["persona_id"].(string)
	if id == "" {
		id = "persona_" + nowStr()
		p["persona_id"] = id
	}
	if fid, ok := p["folder_id"].(string); ok && fid == "" {
		p["folder_id"] = nil
	}
	if _, ok := p["created_at"]; !ok {
		p["created_at"] = nowStr()
	}
	p["updated_at"] = nowStr()
	if _, ok := p["sort_order"]; !ok {
		p["sort_order"] = len(ps.data.Personas)
	}
	for i, existing := range ps.data.Personas {
		if eid, _ := existing["persona_id"].(string); eid == id {
			ps.data.Personas[i] = p
			return ps.saveLocked()
		}
	}
	ps.data.Personas = append(ps.data.Personas, p)
	return ps.saveLocked()
}

func (ps *personaStore) deletePersona(id string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	next := make([]map[string]interface{}, 0, len(ps.data.Personas))
	for _, p := range ps.data.Personas {
		if pid, _ := p["persona_id"].(string); pid != id {
			next = append(next, p)
		}
	}
	ps.data.Personas = next
	return ps.saveLocked()
}

func (ps *personaStore) listFolders(parentID *string) []map[string]interface{} {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	result := make([]map[string]interface{}, 0, len(ps.data.Folders))
	for _, f := range ps.data.Folders {
		if parentID != nil {
			pid, _ := f["parent_id"].(string)
			if *parentID == "" {
				if pid != "" {
					continue
				}
			} else if pid != *parentID {
				continue
			}
		}
		result = append(result, f)
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, _ := result[i]["sort_order"].(float64)
		b, _ := result[j]["sort_order"].(float64)
		return a < b
	})
	return result
}

func (ps *personaStore) getFolder(id string) map[string]interface{} {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, f := range ps.data.Folders {
		if fid, _ := f["folder_id"].(string); fid == id {
			return f
		}
	}
	return nil
}

func (ps *personaStore) upsertFolder(f map[string]interface{}) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	id, _ := f["folder_id"].(string)
	if id == "" {
		id = "folder_" + nowStr()
		f["folder_id"] = id
	}
	if _, ok := f["created_at"]; !ok {
		f["created_at"] = nowStr()
	}
	f["updated_at"] = nowStr()
	if _, ok := f["sort_order"]; !ok {
		f["sort_order"] = len(ps.data.Folders)
	}
	for i, existing := range ps.data.Folders {
		if fid, _ := existing["folder_id"].(string); fid == id {
			ps.data.Folders[i] = f
			return ps.saveLocked()
		}
	}
	ps.data.Folders = append(ps.data.Folders, f)
	return ps.saveLocked()
}

func (ps *personaStore) deleteFolder(id string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	next := make([]map[string]interface{}, 0, len(ps.data.Folders))
	for _, f := range ps.data.Folders {
		if fid, _ := f["folder_id"].(string); fid != id {
			next = append(next, f)
		}
	}
	ps.data.Folders = next
	// Detach personas from the deleted folder
	for _, p := range ps.data.Personas {
		if fid, _ := p["folder_id"].(string); fid == id {
			p["folder_id"] = ""
		}
	}
	return ps.saveLocked()
}

func (ps *personaStore) movePersona(personaID, folderID string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.data.Personas {
		if pid, _ := p["persona_id"].(string); pid == personaID {
			if folderID == "" {
				p["folder_id"] = nil
			} else {
				p["folder_id"] = folderID
			}
			p["updated_at"] = nowStr()
			return ps.saveLocked()
		}
	}
	return nil
}

func (ps *personaStore) reorder(items []map[string]interface{}) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, item := range items {
		id, _ := item["id"].(string)
		typ, _ := item["type"].(string)
		order, _ := item["sort_order"].(float64)
		if typ == "folder" {
			for _, f := range ps.data.Folders {
				if fid, _ := f["folder_id"].(string); fid == id {
					f["sort_order"] = order
				}
			}
		} else {
			for _, p := range ps.data.Personas {
				if pid, _ := p["persona_id"].(string); pid == id {
					p["sort_order"] = order
				}
			}
		}
	}
	return ps.saveLocked()
}

func (ps *personaStore) tree() []map[string]interface{} {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var build func(parentID string) []map[string]interface{}
	build = func(parentID string) []map[string]interface{} {
		nodes := []map[string]interface{}{}
		for _, f := range ps.data.Folders {
			pid, _ := f["parent_id"].(string)
			if pid != parentID {
				continue
			}
			node := map[string]interface{}{}
			for k, v := range f {
				node[k] = v
			}
			node["children"] = build(f["folder_id"].(string))
			nodes = append(nodes, node)
		}
		return nodes
	}
	return build("")
}

// saveLocked persists the store; caller must hold ps.mu.
func (ps *personaStore) saveLocked() error {
	os.MkdirAll(filepath.Dir(ps.path), 0755)
	data, err := json.MarshalIndent(ps.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ps.path, data, 0644)
}
