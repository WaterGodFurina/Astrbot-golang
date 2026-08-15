package aiocqhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthValidExactMatch(t *testing.T) {
	a := &Adapter{Token: "secret123"}
	cases := []struct {
		name       string
		auth       string
		queryToken string
		want       bool
	}{
		// Exact Bearer token.
		{"bearer exact", "Bearer secret123", "", true},
		// Raw token (no scheme) is accepted by design.
		{"raw token", "secret123", "", true},
		// Query token.
		{"query token", "", "secret123", true},
		// Bearer with trailing garbage must be rejected (was accepted before
		// the HasPrefix→ConstantTimeCompare fix).
		{"bearer trailing garbage", "Bearer secret123xyz", "", false},
		{"bearer prefix of token", "Bearer secret12", "", false},
		// Wrong token entirely.
		{"wrong token", "Bearer other", "", false},
		// Empty values.
		{"empty auth", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/ws", nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			if tc.queryToken != "" {
				q := r.URL.Query()
				q.Set("access_token", tc.queryToken)
				r.URL.RawQuery = q.Encode()
			}
			if got := a.authValid(r); got != tc.want {
				t.Errorf("authValid(auth=%q query=%q) = %v, want %v", tc.auth, tc.queryToken, got, tc.want)
			}
		})
	}
}

func TestAuthValidEmptyTokenAllows(t *testing.T) {
	// Per OneBot v11 the token is optional: no token configured → allow.
	a := &Adapter{Token: ""}
	r := httptest.NewRequest("POST", "/ws", nil)
	if !a.authValid(r) {
		t.Fatal("空 token 时事件入口应放行")
	}
}

func TestAuthValidOversizedInputRejected(t *testing.T) {
	a := &Adapter{Token: "secret123"}

	big := strings.Repeat("x", 4097)
	r := httptest.NewRequest("POST", "/ws", nil)
	r.Header.Set("Authorization", "Bearer "+big)
	if a.authValid(r) {
		t.Error("超长 Authorization 头应被拒绝")
	}

	// Oversized query token.
	qr := httptest.NewRequest("POST", "/ws?access_token="+big, nil)
	if a.authValid(qr) {
		t.Error("超长 access_token 参数应被拒绝")
	}

	// Just under the cap with the exact token still works.
	ok := httptest.NewRequest("POST", "/ws", nil)
	ok.Header.Set("Authorization", "Bearer secret123")
	if !a.authValid(ok) {
		t.Error("正常长度且精确匹配的 Bearer token 应放行")
	}
}

// TestHandleHTTPBodySizeLimit verifies the HTTP event endpoint rejects
// oversized bodies with 413 instead of unbounded reads.
func TestHandleHTTPBodySizeLimit(t *testing.T) {
	a := &Adapter{Token: ""}               // token 可选：无 token 配置时任何请求都会走到 body 读取
	body := strings.Repeat("a", (1<<20)+2) // 超过 1MiB 上限
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	a.handleHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("超大 body 应返回 413，实际 %d", w.Code)
	}
}

// TestHandleHTTPAuthOrdering verifies auth is enforced before any body decode,
// and that valid requests pass through.
func TestHandleHTTPAuthOrdering(t *testing.T) {
	validEvent := `{"post_type":"meta_event","meta_event_type":"heartbeat"}`

	t.Run("no token configured accepts", func(t *testing.T) {
		a := &Adapter{Token: ""}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(validEvent))
		a.handleHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("无 token 配置时合法事件应返回 200，实际 %d", w.Code)
		}
	})

	t.Run("wrong token rejected", func(t *testing.T) {
		a := &Adapter{Token: "secret"}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(validEvent))
		r.Header.Set("Authorization", "Bearer wrong")
		a.handleHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("错误 token 应返回 401，实际 %d", w.Code)
		}
	})

	t.Run("correct token accepted", func(t *testing.T) {
		a := &Adapter{Token: "secret"}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(validEvent))
		r.Header.Set("Authorization", "Bearer secret")
		a.handleHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("正确 token 应返回 200，实际 %d", w.Code)
		}
	})

	t.Run("invalid json rejected", func(t *testing.T) {
		a := &Adapter{Token: ""}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader("not-json{"))
		a.handleHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("非法 JSON 应返回 400，实际 %d", w.Code)
		}
	})

	t.Run("oversized body rejected even with valid token", func(t *testing.T) {
		a := &Adapter{Token: "secret"}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("a", (1<<20)+2)))
		r.Header.Set("Authorization", "Bearer secret")
		a.handleHTTP(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("已认证的大 body 也应返回 413，实际 %d", w.Code)
		}
	})
}
