package telegram

import "testing"

func TestTelegramifyMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello_world", `hello\_world`},
		{"a.b!c", `a\.b\!c`},
		{"**bold**", `*bold*`},
		{"*italic*", `_italic_`},
		{"__bold__", `*bold*`},
		{"~~del~~", `~del~`},
		{"`code_here`", "`code_here`"},
		{"plain text", "plain text"},
		{"[text](https://a.b/c)", `[text](https://a.b/c)`},
	}
	for _, c := range cases {
		if got := telegramifyMarkdown(c.in); got != c.want {
			t.Errorf("telegramifyMarkdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
