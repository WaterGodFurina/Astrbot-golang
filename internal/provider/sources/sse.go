package sources

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
)

// sseReader parses a Server-Sent Events stream line by line, invoking
// onData for every `data:` payload (including the sentinel "[DONE]").
type sseReader struct {
	scanner *bufio.Scanner
	onData  func(data string) (stop bool)
}

// newSSEReader wraps an SSE response body. The caller must close resp.Body.
func newSSEReader(ctx context.Context, resp *http.Response, onData func(data string) (stop bool)) *sseReader {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &sseReader{scanner: scanner, onData: onData}
}

// scan reads the stream until done, EOF, or an error. The stream body is NOT
// closed here so callers control resp.Body lifetime.
func (r *sseReader) scan() error {
	for r.scanner.Scan() {
		line := strings.TrimSpace(r.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil
		}
		if data != "" {
			if r.onData(data) {
				return nil
			}
		}
	}
	if err := r.scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}
