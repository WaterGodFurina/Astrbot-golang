package contentsafety

import "testing"

func TestStrategySelectorKeyword(t *testing.T) {
	cfg := map[string]interface{}{
		"internal_keywords": map[string]interface{}{
			"enable":         true,
			"extra_keywords": []interface{}{"敏感词"},
		},
		"baidu_aip": map[string]interface{}{"enable": false},
	}
	s := NewStrategySelector(cfg)
	if !s.IsEnabled() {
		t.Fatal("keyword strategy should be enabled")
	}
	if ok, _ := s.Check("没有敏感内容"); !ok {
		t.Fatal("clean text should pass")
	}
	if ok, _ := s.Check("这里有敏感词"); ok {
		t.Fatal("keyword text should be blocked")
	}
}

func TestStrategySelectorDisabled(t *testing.T) {
	s := NewStrategySelector(map[string]interface{}{
		"internal_keywords": map[string]interface{}{"enable": false},
		"baidu_aip":         map[string]interface{}{"enable": false},
	})
	if s.IsEnabled() {
		t.Fatal("no strategy should be enabled")
	}
	if ok, _ := s.Check("任何内容"); !ok {
		t.Fatal("disabled selector always passes")
	}
}

func TestBaiduAipCheckerErrorSemantics(t *testing.T) {
	c := NewBaiduAipChecker("1", "bad", "bad")
	ok, _ := c.Check("hello")
	if baiduAipSDKEnabled {
		// 真实 SDK 构建：传输/鉴权失败必须 fail-closed（对齐 Python
		// baidu_aip.py：缺少 conclusionType 即判不合规）。
		if ok {
			t.Fatal("baidu aip sdk transport/auth errors must fail closed")
		}
		return
	}
	// 默认构建（未编译 SDK）：占位实现 fail-open 并提示如何启用。
	if !ok {
		t.Fatal("baidu aip placeholder should fail open")
	}
}
