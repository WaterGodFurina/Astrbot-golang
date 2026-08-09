package sources

// configString returns the string value of key, or the fallback when absent.
func configString(config map[string]interface{}, key, fallback string) string {
	if v, ok := config[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// configInt returns the int value of key, or the fallback when absent.
func configInt(config map[string]interface{}, key string, fallback int) int {
	switch v := config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return fallback
}
