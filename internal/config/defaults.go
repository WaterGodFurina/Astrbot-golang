// Package config - default configuration schema.
// Ported from astrbot/core/config/default.py
package config

// DefaultConfig returns the default configuration schema.
func DefaultConfig() *SchemaNode {
	return &SchemaNode{
		Type: SchemaObject,
		Children: map[string]*SchemaNode{
			"plugin_idle_unload_minutes": {
				Type:         SchemaInt,
				DefaultValue: 0,
				Optional:     true,
			},
			"platform": {
				Type:     SchemaList,
				Optional: true,
				Items: &SchemaNode{
					Type: SchemaObject,
				},
			},
			"provider": {
				Type:     SchemaList,
				Optional: true,
				Items: &SchemaNode{
					Type: SchemaObject,
				},
			},
			"plugin": {
				Type:     SchemaList,
				Optional: true,
				Items: &SchemaNode{
					Type: SchemaObject,
				},
			},
			"wake_prefix": {
				Type:         SchemaString,
				DefaultValue: "",
				Optional:     true,
			},
			"log_level": {
				Type:         SchemaString,
				DefaultValue: "INFO",
				Optional:     true,
			},
			"dashboard": {
				Type:     SchemaObject,
				Optional: true,
				Children: map[string]*SchemaNode{
					"username": {
						Type:         SchemaString,
						DefaultValue: "admin",
						Optional:     true,
					},
					"password": {
						Type:         SchemaString,
						DefaultValue: "",
						Optional:     true,
					},
					"port": {
						Type:         SchemaInt,
						DefaultValue: 6185,
						Optional:     true,
					},
				},
			},
			"provider_settings": {
				Type:     SchemaObject,
				Optional: true,
				Children: map[string]*SchemaNode{
					"max_context_length": {
						Type:         SchemaInt,
						DefaultValue: 50,
						Optional:     true,
					},
					"dequeue_context_length": {
						Type:         SchemaInt,
						DefaultValue: 10,
						Optional:     true,
					},
					"prompt_prefix": {
						Type:         SchemaString,
						DefaultValue: "",
						Optional:     true,
					},
				},
			},
			"knowledge_base": {
				Type:     SchemaObject,
				Optional: true,
			},
		},
	}
}
