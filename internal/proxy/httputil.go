package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// --- shared HTTP helpers (copied from minimax-proxy to keep no deps) ---

func IsRateLimitError(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "rate_limit_error") ||
		strings.Contains(lower, "token plan usage limit") ||
		strings.Contains(lower, "usage limit reached")
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
