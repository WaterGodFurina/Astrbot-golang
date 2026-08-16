package plugin

// ConfigResolver is the single entry point for resolving a plugin's effective
// configuration. Every plugin-config read path — the HostService GetConfig
// reverse RPC (what Go/Python plugins see via sdk.Host.GetConfig), the
// dashboard plugin-config dialog (GET /api/v1/plugins/config), and direct
// manager reads — goes through it, so no caller can observe a different
// default-value layering.
//
// The layering, from the bottom up, is:
//
//	Defaults (from the config schema) ← stored config.json ← (future) runtime override
//
// The stored config (whose packaged-metadata keys LoadConfig already strips)
// wins over the recursively-injected schema defaults: an empty config.json
// yields the full schema default set, while a saved config keeps its stored
// values with missing keys filled in. Any future runtime override must be
// layered above the stored config inside ResolvePluginConfig, keeping the
// merge logic in exactly one place.
type ConfigResolver struct {
	// loadStored returns the plugin's stored config.json (metadata keys
	// already stripped), or an empty map when the file is absent/unreadable.
	loadStored func(name string) map[string]interface{}
	// flatSchema returns the plugin's config schema in the WebUI "items"
	// shape (properties→items normalized, JSON-Schema types mapped), or an
	// empty map.
	flatSchema func(name string) map[string]interface{}
}

// NewConfigResolver builds a resolver over the given data sources. Nil
// sources degrade to empty inputs, so the resolver stays safe for partial
// wiring (e.g. before a manager is constructed).
func NewConfigResolver(loadStored, flatSchema func(name string) map[string]interface{}) *ConfigResolver {
	return &ConfigResolver{loadStored: loadStored, flatSchema: flatSchema}
}

// ResolvePluginConfig returns the plugin's effective config: schema defaults
// (recursive, including nested object groups) merged under the stored config.
func (r *ConfigResolver) ResolvePluginConfig(name string) map[string]interface{} {
	cfg := map[string]interface{}{}
	if r.loadStored != nil {
		if stored := r.loadStored(name); stored != nil {
			cfg = stored
		}
	}
	if r.flatSchema != nil {
		mergeSchemaDefaults(cfg, r.flatSchema(name))
	}
	return cfg
}

// ConfigResolver returns a manager-bound resolver used as the single entry
// for every plugin-config read (HostService.GetConfig, dashboard config
// dialog, SDK-side reads).
func (m *SubprocessManager) ConfigResolver() *ConfigResolver {
	return NewConfigResolver(m.LoadConfig, m.FlatSchema)
}

// mergeSchemaDefaults fills cfg with each schema key's "default" value (and
// recurses into object groups) when the key is absent. Keys already present
// in cfg (stored config values) are never overwritten.
func mergeSchemaDefaults(cfg, schema map[string]interface{}) {
	for key, metaAny := range schema {
		meta, ok := metaAny.(map[string]interface{})
		if !ok {
			continue
		}
		if def, ok := meta["default"]; ok {
			if _, exists := cfg[key]; !exists {
				cfg[key] = def
			}
			continue
		}
		if itemsAny, ok := meta["items"].(map[string]interface{}); ok {
			cur, _ := cfg[key].(map[string]interface{})
			if cur == nil {
				cur = map[string]interface{}{}
			}
			mergeSchemaDefaults(cur, itemsAny)
			if len(cur) > 0 {
				cfg[key] = cur
			}
		}
	}
}
