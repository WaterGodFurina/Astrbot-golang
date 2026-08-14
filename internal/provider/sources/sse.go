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
//
// Per the SSE spec a single event may span several `data:` lines (joined with
// a newline) and is terminated by a blank line; events are dispatched to onData
// one at a time. The ctx parameter is intentionally not consulted here: the
// blocking scanner cannot be interrupted mid-read, and callers already bound
// the stream lifetime via their request context or http.Client timeout, so
// checking ctx here would only add a no-op.
func (r *sseReader) scan() error {
	var dataLines []string
	dispatch := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if data == "" {
			return false, nil
		}
		if data == "[DONE]" {
			return true, nil
		}
		return r.onData(data), nil
	}
	for r.scanner.Scan() {
		line := strings.TrimSpace(r.scanner.Text())
		if line == "" {
			if stop, err := dispatch(); err != nil || stop {
				return err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	if stop, err := dispatch(); err != nil || stop {
		return err
	}
	if err := r.scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}
