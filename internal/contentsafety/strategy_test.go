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

func TestBaiduAipCheckerFailOpen(t *testing.T) {
	c := NewBaiduAipChecker("1", "bad", "bad")
	// 无网络环境：token 获取失败应 fail-open（返回 ok）
	if ok, _ := c.Check("hello"); !ok {
		t.Fatal("baidu aip transport errors should fail open")
	}
}
