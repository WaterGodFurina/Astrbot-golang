// Order-preserving JSON support.
//
// Go's encoding/json marshals map[string]interface{} with alphabetically
// sorted keys, which destroys the ordering of the Python source dicts this
// project ports (config metadata, default config, ...). AstrBot's WebUI
// renders config sections/fields by iterating the JSON keys of the served
// metadata, so alphabetical order makes the config page look scrambled.
//
// OrderedJSON preserves key insertion order through an unmarshal/marshal
// round-trip, mirroring Python dict ordering.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// OrderedJSON is a JSON object whose keys keep their source order.
// Nested objects are also preserved as OrderedJSON.
type OrderedJSON struct {
	keys   []string
	values map[string]interface{}
}

// NewOrderedJSON returns an empty ordered object.
func NewOrderedJSON() *OrderedJSON {
	return &OrderedJSON{values: map[string]interface{}{}}
}

// FromMap builds an ordered object from a plain map. Nested maps are
// converted recursively. Key order is sorted for determinism (callers that
// need a specific order should build the object directly or rely on
// UnmarshalJSON).
func FromMap(m map[string]interface{}) *OrderedJSON {
	o := NewOrderedJSON()
	if m == nil {
		return o
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, orderedValue(m[k]))
	}
	return o
}

// orderedValue recursively converts maps/slices so ordering is preserved
// at every level.
func orderedValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return FromMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = orderedValue(e)
		}
		return out
	default:
		return v
	}
}

// UnmarshalJSON decodes preserving key order (using a token stream).
func (o *OrderedJSON) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	// Read opening brace.
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("OrderedJSON: expected object, got %v", tok)
	}
	o.keys = nil
	o.values = map[string]interface{}{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return fmt.Errorf("OrderedJSON: expected string key, got %v", kt)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		val, err := parseOrderedJSONValue(raw)
		if err != nil {
			return err
		}
		o.keys = append(o.keys, key)
		o.values[key] = val
	}
	// Consume closing brace.
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// parseOrderedJSONValue parses a raw JSON value into an ordered structure.
func parseOrderedJSONValue(raw json.RawMessage) (interface{}, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	switch trimmed[0] {
	case '{':
		var o OrderedJSON
		if err := o.UnmarshalJSON(trimmed); err != nil {
			return nil, err
		}
		return &o, nil
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		out := make([]interface{}, len(arr))
		for i, e := range arr {
			v, err := parseOrderedJSONValue(e)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	default:
		// #nosec unsafe-deserialization-interface -- 配置文件解析：OrderedJSON 需保留任意 JSON 值
		//（字符串/数字/布尔/空），无法用具体结构体；数据来自受信任的本地配置文件。
		var v interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return nil, err
		}
		return v, nil
	}
}

// MarshalJSON emits keys in stored order.
func (o *OrderedJSON) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(o.values[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Keys returns the keys in stored order.
func (o *OrderedJSON) Keys() []string {
	return o.keys
}

// Get returns the value for a key.
func (o *OrderedJSON) Get(key string) (interface{}, bool) {
	v, ok := o.values[key]
	return v, ok
}

// Set assigns a value, appending the key if new.
func (o *OrderedJSON) Set(key string, value interface{}) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = orderedValue(value)
}

// Delete removes a key.
func (o *OrderedJSON) Delete(key string) {
	if _, exists := o.values[key]; !exists {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len returns the number of keys.
func (o *OrderedJSON) Len() int {
	return len(o.keys)
}

// Has reports whether a key exists.
func (o *OrderedJSON) Has(key string) bool {
	_, ok := o.values[key]
	return ok
}

// Map returns a plain (unordered) map representation.
func (o *OrderedJSON) Map() map[string]interface{} {
	out := make(map[string]interface{}, len(o.keys))
	for k, v := range o.values {
		out[k] = v
	}
	return out
}

// GetOrderedJSON returns the value as *OrderedJSON if it is one.
func GetOrderedJSON(v interface{}) (*OrderedJSON, bool) {
	o, ok := v.(*OrderedJSON)
	return o, ok
}

// ParseOrderedJSON parses data into an ordered JSON object.
func ParseOrderedJSON(data []byte) (*OrderedJSON, error) {
	var o OrderedJSON
	if err := o.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return &o, nil
}
