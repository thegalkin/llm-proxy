package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// --- shared HTTP helpers (copied from minimax-proxy to keep no deps) ---

// IsRateLimitError reports whether an upstream 429 body looks like a real
// rate-limit / quota-exhausted error (rather than e.g. "Model is not
// supported"). The proxy uses this to decide whether to mark the key as
// cooling down and try the next one, or to forward the error to the client
// as a terminal failure.
func IsRateLimitError(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "rate_limit_error") ||
		strings.Contains(lower, "token plan usage limit") ||
		strings.Contains(lower, "usage limit reached") ||
		strings.Contains(lower, "gousagelimiterror")
}

// IsModelShapeError reports whether a 4xx body means "the request is fine but
// this upstream does not serve that model" — e.g. opencode-go returns 401
// ModelError when a key does not have the requested model. Rotating to
// another opencode-go key with the same model is pointless, so the proxy
// surfaces the error to the client instead of cycling through every key and
// returning a misleading "all keys exhausted".
func IsModelShapeError(statusCode int, body []byte) bool {
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusBadRequest && statusCode != http.StatusForbidden {
		return false
	}
	lower := strings.ToLower(string(body))
	// Quota / auth signals are NOT a model-shape error — the next key in
	// the same family might still serve the model. Only treat as a
	// terminal "this upstream does not serve that model" if the body
	// carries no quota/auth hint.
	if strings.Contains(lower, "gousagelimiterror") ||
		strings.Contains(lower, "usage limit") ||
		strings.Contains(lower, "rate_limit_error") ||
		strings.Contains(lower, "creditserror") ||
		strings.Contains(lower, "autherror") {
		return false
	}
	// Empty model name in the error message ("Model  is not supported"
	// with two spaces) is what opencode-go upstream produces when it
	// rejects a request without naming the model — typically because
	// quota was already exhausted on that key and the upstream short-
	// circuited before checking the model name. The next key in the
	// family may still serve the request, so this is not a model-shape
	// error: keep rotating.
	if strings.Contains(lower, "model  is not supported") ||
		strings.Contains(lower, "model   is not supported") {
		return false
	}
	return strings.Contains(lower, "modelerror") ||
		strings.Contains(lower, "is not supported") ||
		strings.Contains(lower, "model_not_found")
}

func CopyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailers", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func ContainsHeader(h http.Header, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

type FlushWriter struct {
	w  http.ResponseWriter
	f  http.Flusher
	WW *bufio.Writer
}

func newFlushWriter(w http.ResponseWriter) *FlushWriter {
	fw := &FlushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.f = f
	}
	fw.WW = bufio.NewWriter(w)
	return fw
}

func (fw *FlushWriter) Write(p []byte) (int, error) {
	n, err := fw.WW.Write(p)
	if err != nil {
		return n, err
	}
	if err := fw.WW.Flush(); err != nil {
		return n, err
	}
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, nil
}

type PeekBuf struct {
	Peek *[]byte
	Cap  int
}

func (p *PeekBuf) Write(b []byte) (int, error) {
	rem := p.Cap - len(*p.Peek)
	if rem > 0 {
		take := len(b)
		if take > rem {
			take = rem
		}
		*p.Peek = append(*p.Peek, b[:take]...)
	}
	return len(b), nil
}

func StreamSSE(dst *FlushWriter, src io.Reader) error {
	const (
		eventTerminator = "\n\n"
		eventDataPrefix = "data: "
		eventDone       = "[DONE]"
	)
	br := bufio.NewReaderSize(src, 64*1024)
	var (
		event []byte
		line  []byte
		err   error
	)
	for {
		line, err = br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return err
		}
		event = append(event, line...)
		if err == io.EOF {
			break
		}
		if !bytes.HasSuffix(event, []byte(eventTerminator)) {
			continue
		}
		payload := event
		if i := bytes.Index(payload, []byte(eventDataPrefix)); i >= 0 {
			data := payload[i+len(eventDataPrefix):]
			if j := bytes.IndexByte(data, '\n'); j >= 0 {
				data = data[:j]
			}
			if bytes.Equal(bytes.TrimSpace(data), []byte(eventDone)) {
				event = event[:0]
				continue
			}
		}
		if _, werr := dst.Write(event); werr != nil {
			return werr
		}
		event = event[:0]
	}
	if len(event) > 0 {
		if _, werr := dst.Write(event); werr != nil {
			return werr
		}
	}
	return nil
}
