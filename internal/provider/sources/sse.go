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
	ctx     context.Context
	scanner *bufio.Scanner
	onData  func(data string) (stop bool)
}

// newSSEReader wraps an SSE response body. The caller must close resp.Body.
func newSSEReader(ctx context.Context, resp *http.Response, onData func(data string) (stop bool)) *sseReader {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &sseReader{ctx: ctx, scanner: scanner, onData: onData}
}

// scan reads the stream until done, EOF, or an error. The stream body is NOT
// closed here so callers control resp.Body lifetime.
//
// Per the SSE spec a single event may span several `data:` lines (joined with
// a newline) and is terminated by a blank line; events are dispatched to onData
// one at a time.
//
// The context is honored at token boundaries: bufio.Scanner.Scan() blocks on
// the underlying read and cannot be interrupted mid-read, so the cancelled
// context is checked at the top of every loop iteration. A cancellation that
// arrives between tokens stops the scan at the next boundary (or immediately
// when the stream is idle), so the goroutine can never keep looping forever
// after cancellation. While a read is blocked mid-token, cancellation still
// surfaces through the HTTP transport — the request carries ctx, and the
// http.Client used for streaming bounds the whole read — but this loop check
// guarantees progress even when the transport does not.
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
	for {
		select {
		case <-r.ctx.Done():
			return nil
		default:
		}
		if !r.scanner.Scan() {
			break
		}
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
