package handlers

// HTTP response compression.
//
// The portal ships its entire SPA as one large index.html (~1.5 MB) plus a
// stylesheet. Served raw that is slow over a remote VPN; gzip cuts text
// payloads by ~80-85%. This middleware compresses text-ish responses for
// clients that advertise gzip support.
//
// Safety:
//   - WebSocket upgrades are passed through untouched — those handlers hijack
//     the connection and must see the raw ResponseWriter.
//   - Only text content types are compressed (decided from the handler's
//     Content-Type); images and binary downloads stream through unchanged.
//   - Flush is delegated so SSE / streaming endpoints keep working; Hijack is
//     delegated as defensive insurance.
//   - Uses only the standard library (compress/gzip).

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// gzipWriterPool reuses gzip.Writers so a busy portal doesn't allocate one
// per request.
var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return w
	},
}

// gzipCompressibleCT lists the Content-Type prefixes worth compressing.
// Everything else (images, video, already-compressed downloads) is left as-is.
var gzipCompressibleCT = []string{
	"text/html", "text/css", "text/plain", "text/xml", "text/javascript",
	"application/javascript", "application/json", "application/xml",
	"application/manifest+json", "image/svg+xml",
}

func ctIsCompressible(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	for _, p := range gzipCompressibleCT {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

// gzipResponseWriter lazily gzip-encodes the response body. The decision to
// compress is made on WriteHeader, from the Content-Type the handler set —
// so binary responses pass through and only text payloads are compressed.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	compress    bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	h := w.Header()
	// Only compress a normal 200 body: skip 204/304/redirects (no/!modified
	// body), skip when the handler already chose an encoding, and skip
	// non-text content types.
	if status == http.StatusOK &&
		h.Get("Content-Encoding") == "" &&
		ctIsCompressible(h.Get("Content-Type")) {
		w.compress = true
		h.Del("Content-Length") // the byte count changes after compression
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w.ResponseWriter)
		w.gz = gz
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compress {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (SSE, progress logs) working — flush the
// gzip layer first so buffered compressed bytes reach the client.
func (w *gzipResponseWriter) Flush() {
	if w.compress && w.gz != nil {
		w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying writer. WebSocket requests are already
// excluded by GzipResponses, but this keeps any other hijacking handler safe.
func (w *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *gzipResponseWriter) finish() {
	if w.compress && w.gz != nil {
		w.gz.Close()
		gzipWriterPool.Put(w.gz)
		w.gz = nil
	}
}

// GzipResponses is the response-compression middleware. Wire it into the
// router with r.Use(GzipResponses).
func GzipResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pass through when the client can't take gzip, or when this is a
		// WebSocket handshake (those handlers hijack the raw connection).
		if !strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") ||
			strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}
