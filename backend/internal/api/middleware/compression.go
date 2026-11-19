package middleware

import (
	"bufio"
	"compress/gzip"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Limits of the compression module.
const (
	// DefaultCompressionMinSize is the smallest response body in bytes that is
	// compressed (COMPRESSION_MIN_SIZE default).
	DefaultCompressionMinSize = 1024
	// MaxCompressionMinSize is the largest COMPRESSION_MIN_SIZE (1 MiB): the
	// middleware holds up to that many bytes of a response back to decide.
	MaxCompressionMinSize = 1 << 20
	// compressionLevel is the gzip level: a balance of ratio and CPU, not
	// configurable.
	compressionLevel = 5
)

const (
	encodingGzip = "gzip"
	headerVary   = "Vary"
)

// CompressionOptions are the settings of Compression.
type CompressionOptions struct {
	// MinSize is the smallest uncompressed body in bytes that is compressed.
	// Zero or less means DefaultCompressionMinSize.
	MinSize int
}

// gzipWriters pools the gzip writers (each holds about 1 MiB of state).
var gzipWriters = sync.Pool{
	New: func() any {
		// The level is a valid constant: NewWriterLevel cannot fail.
		w, _ := gzip.NewWriterLevel(io.Discard, compressionLevel)
		return w
	},
}

// Compression gzips a response when the client asks for it (Accept-Encoding
// lists gzip with a q-value above 0; "*" does not count). A response is
// compressed only when all of these hold:
//
//   - the method is not HEAD, the status allows a body (not 1xx, 204, 304) and
//     it is not 206 (no Content-Range either);
//   - the handler set no Content-Encoding and no "Cache-Control: no-transform";
//   - the content type is compressible: text/*, JSON, XML, JavaScript and SVG
//     (see compressibleType). Without a Content-Type header the type is
//     sniffed from the first bytes, never taken on trust;
//   - the body reaches MinSize bytes: the declared Content-Length when there
//     is one, otherwise the bytes written so far (they are held back until the
//     size is known). A smaller body is sent as it was, with its length.
//
// A compressed response has Content-Encoding: gzip, no Content-Length and a
// weak ETag. Vary: Accept-Encoding is added to every response whose type is
// compressible, compressed or not, so a cache never mixes the variants up.
// A Flush starts the stream at once (a response that is flushed is a stream:
// its size is unknown) and every later Flush is a gzip sync flush.
//
// A WebSocket request (path /ws or an Upgrade: websocket header) is never
// wrapped, so the upgrade (Hijack) is untouched. A request body is never
// decompressed: the module only handles responses. After a panic in the
// chain before the stream started, the writer is put back, so Recovery
// (outside) answers with a plain 500. After the stream started the status is
// already sent: the stream is left without its gzip trailer (the client's
// reader fails with an unexpected EOF) and the 500 body is dropped.
//
// Register it right after Recovery and before everything that writes. The
// modules inside (idempotency, audit) see the uncompressed bytes.
func Compression(opts CompressionOptions) gin.HandlerFunc {
	minSize := opts.MinSize
	if minSize <= 0 {
		minSize = DefaultCompressionMinSize
	}

	return func(c *gin.Context) {
		if isWebSocketRequest(c.Request) {
			c.Next()
			return
		}

		orig := c.Writer
		cw := &compressWriter{
			ResponseWriter: orig,
			minSize:        minSize,
			threshold:      minSize,
			size:           -1,
			accepts:        c.Request.Method != http.MethodHead && acceptsGzip(c.Request.Header.Values("Accept-Encoding")),
		}
		c.Writer = cw

		finished := false
		defer func() {
			if finished {
				return
			}
			// A panic in the chain. Recovery, outside, answers it.
			if cw.state == stateGzip {
				// Part of the response is out. Do not end the gzip stream: without
				// its trailer the client's reader fails with an unexpected EOF
				// instead of taking a cut response for a complete one. What Recovery
				// writes is dropped, since it would be taken for gzip data. The
				// writer stays.
				cw.abandonGzip()
				return
			}
			cw.buf = nil
			c.Writer = orig
		}()

		c.Next()
		finished = true
		cw.finish()
		c.Writer = orig
	}
}

// isWebSocketRequest reports whether r is for /ws or asks for an upgrade to a
// WebSocket.
func isWebSocketRequest(r *http.Request) bool {
	if p := r.URL.Path; p == "/ws" || strings.HasPrefix(p, "/ws/") {
		return true
	}
	for _, v := range r.Header.Values("Upgrade") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
}

// acceptsGzip reports whether the Accept-Encoding header lines list gzip with
// a q-value above 0. "*" is not treated as gzip, and one refusal (q=0) wins
// over an offer. An entry with a q-value that does not parse is ignored.
func acceptsGzip(values []string) bool {
	accepted := false
	for _, v := range values {
		for _, entry := range strings.Split(v, ",") {
			parts := strings.Split(entry, ";")
			if !strings.EqualFold(strings.TrimSpace(parts[0]), encodingGzip) {
				continue
			}
			q := 1.0
			valid := true
			for _, param := range parts[1:] {
				name, val, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
					continue
				}
				f, ok := parseQValue(val)
				if !ok {
					valid = false
					break
				}
				q = f
			}
			switch {
			case !valid:
			case q == 0:
				return false
			default:
				accepted = true
			}
		}
	}
	return accepted
}

// parseQValue parses a q-value in the syntax of RFC 9110: "0" with up to
// three decimals, or "1" with up to three zeros. Nothing else (no sign, no
// exponent, no NaN, no white space) is valid.
func parseQValue(s string) (float64, bool) {
	if s == "" || (s[0] != '0' && s[0] != '1') {
		return 0, false
	}
	if len(s) == 1 {
		return float64(s[0] - '0'), true
	}
	if s[1] != '.' || len(s) > 5 {
		return 0, false
	}
	for _, d := range []byte(s[2:]) {
		if d < '0' || d > '9' || (s[0] == '1' && d != '0') {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// compressibleType reports whether a Content-Type is worth compressing: an
// allowlist, so an already compressed format (images, archives, video) is
// never touched.
func compressibleType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"),
		mt == "application/json", strings.HasSuffix(mt, "+json"),
		mt == "application/xml", strings.HasSuffix(mt, "+xml"),
		mt == "application/javascript":
		return true
	}
	return false
}

// bodyAllowed reports whether a response with this status has a body.
func bodyAllowed(status int) bool {
	return !(status >= 100 && status < 200) && status != http.StatusNoContent && status != http.StatusNotModified
}

// addVary adds Vary: Accept-Encoding unless a Vary line already lists it (or
// "*").
func addVary(h http.Header) {
	for _, line := range h.Values(headerVary) {
		for _, token := range strings.Split(line, ",") {
			token = strings.TrimSpace(token)
			if token == "*" || strings.EqualFold(token, "Accept-Encoding") {
				return
			}
		}
	}
	h.Add(headerVary, "Accept-Encoding")
}

// hasNoTransform reports whether Cache-Control forbids transforming the body.
func hasNoTransform(h http.Header) bool {
	for _, line := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-transform") {
				return true
			}
		}
	}
	return false
}

// compressState is where a response is on its way to the client.
type compressState uint8

const (
	// statePending: nothing is decided yet.
	statePending compressState = iota
	// stateBuffering: the response is compressible and eligible; bytes are held
	// back until MinSize is reached, a flush or the end.
	stateBuffering
	// stateGzip: the headers and the first bytes are out, gzip-encoded.
	stateGzip
	// statePass: the response goes to the client as the handler writes it.
	statePass
)

// compressWriter wraps gin.ResponseWriter. It keeps the semantics that the
// chain relies on: Status is the real writer's, Size and Written describe
// what the handler wrote (uncompressed), and WriteHeader after the first
// write is ignored.
type compressWriter struct {
	gin.ResponseWriter
	gz *gzip.Writer
	// sniffedType is the Content-Type found by sniffing; it is set on the
	// response only when the body is compressed (net/http would otherwise
	// sniff the gzip bytes).
	sniffedType string
	// buf holds the bytes that are held back until the size is known.
	buf []byte
	// size is the number of bytes the handler wrote, -1 before any write.
	size int
	// minSize is MinSize; threshold is the size at which the body is
	// compressed (0 when a declared Content-Length is already large enough).
	minSize   int
	threshold int
	state     compressState
	accepts   bool
	// dead is true after the gzip stream was closed or abandoned: later
	// writes are dropped.
	dead bool
}

// begin decides what to do with the response from its status and headers and,
// for a response without a Content-Type, from its first bytes b. It runs once,
// at the first write, flush or end of the response.
func (w *compressWriter) begin(b []byte) {
	w.state = statePass
	if !bodyAllowed(w.Status()) {
		return
	}
	h := w.Header()
	contentType := h.Get("Content-Type")
	sniffed := false
	if contentType == "" {
		if len(b) == 0 {
			return
		}
		contentType = http.DetectContentType(b)
		sniffed = true
	}
	if !compressibleType(contentType) {
		return
	}
	// The answer depends on Accept-Encoding from here on, whatever the
	// request asked.
	addVary(h)
	if !w.accepts || w.Status() == http.StatusPartialContent || h.Get("Content-Range") != "" || hasNoTransform(h) {
		return
	}
	if enc := h.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return
	}
	if cl := h.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
		if err != nil || n < int64(w.minSize) {
			return
		}
		w.threshold = 0
	}
	if sniffed {
		w.sniffedType = contentType
	}
	w.state = stateBuffering
}

// startGzip sends the headers of a compressed response and what was held
// back.
func (w *compressWriter) startGzip() error {
	h := w.Header()
	if w.sniffedType != "" {
		h.Set("Content-Type", w.sniffedType)
	}
	h.Del("Content-Length")
	h.Set("Content-Encoding", encodingGzip)
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		h.Set("ETag", "W/"+etag)
	}

	gz, _ := gzipWriters.Get().(*gzip.Writer)
	gz.Reset(w.ResponseWriter)
	w.gz = gz
	w.state = stateGzip

	held := w.buf
	w.buf = nil
	if len(held) == 0 {
		return nil
	}
	_, err := gz.Write(held)
	return err
}

// closeGzip ends the gzip stream and returns the writer to the pool.
func (w *compressWriter) closeGzip() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	w.gz.Reset(io.Discard)
	gzipWriters.Put(w.gz)
	w.gz = nil
	w.dead = true
}

// abandonGzip drops the gzip stream without writing its trailer and returns
// the writer to the pool. Later writes are dropped.
func (w *compressWriter) abandonGzip() {
	if w.gz == nil {
		return
	}
	w.gz.Reset(io.Discard)
	gzipWriters.Put(w.gz)
	w.gz = nil
	w.dead = true
}

// finish sends what is held back, as it was when the body stayed under the
// minimum, and ends the gzip stream. It does nothing when the handler wrote
// nothing: Gin then commits the response itself (and writes its own 404
// body).
func (w *compressWriter) finish() {
	switch w.state {
	case stateGzip:
		w.closeGzip()
	case statePending, stateBuffering:
		if w.size < 0 {
			return
		}
		if w.state == statePending {
			w.begin(nil)
		}
		w.state = statePass
		held := w.buf
		w.buf = nil
		if len(held) > 0 {
			_, _ = w.ResponseWriter.Write(held)
		} else {
			w.ResponseWriter.WriteHeaderNow()
		}
	case statePass:
	}
}

func (w *compressWriter) WriteHeader(code int) {
	if w.dead {
		return
	}
	// Held back, the real writer has not seen a write, so the "no change
	// after the first write" rule of Gin is applied here.
	if w.size >= 0 && (w.state == statePending || w.state == stateBuffering) {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *compressWriter) WriteHeaderNow() {
	if w.dead {
		return
	}
	switch w.state {
	case statePending, stateBuffering:
		// The body decides, so the response is not committed yet.
		if w.size < 0 {
			w.size = 0
		}
	default:
		if w.size < 0 {
			w.size = 0
		}
		w.ResponseWriter.WriteHeaderNow()
	}
}

func (w *compressWriter) Write(b []byte) (int, error) {
	if w.dead {
		return len(b), nil
	}
	if w.size < 0 {
		w.size = 0
	}
	if w.state == statePending && len(b) > 0 {
		w.begin(b)
	}

	var (
		n   int
		err error
	)
	switch w.state {
	case statePass:
		n, err = w.ResponseWriter.Write(b)
	case stateGzip:
		n, err = w.gz.Write(b)
	case stateBuffering:
		if len(w.buf)+len(b) < w.threshold {
			w.buf = append(w.buf, b...)
			n = len(b)
			break
		}
		if err = w.startGzip(); err == nil {
			n, err = w.gz.Write(b)
		}
	case statePending:
		// An empty write: nothing to decide on.
	}
	w.size += n
	return n, err
}

func (w *compressWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Flush sends what is held back. A flushed response is a stream, so it is
// compressed from the first flush on (when it is eligible) and every flush
// is a gzip sync flush.
func (w *compressWriter) Flush() {
	if w.dead {
		return
	}
	if w.size < 0 {
		w.size = 0
	}
	if w.state == statePending {
		w.begin(nil)
	}
	if w.state == stateBuffering {
		if err := w.startGzip(); err != nil {
			return
		}
	}
	if w.state == stateGzip {
		if err := w.gz.Flush(); err != nil {
			return
		}
	}
	w.ResponseWriter.Flush()
}

// Hijack hands the connection over; nothing is compressed after it.
func (w *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.state != stateGzip {
		w.state = statePass
		w.buf = nil
	}
	if w.size < 0 {
		w.size = 0
	}
	return w.ResponseWriter.Hijack()
}

func (w *compressWriter) Size() int {
	return w.size
}

func (w *compressWriter) Written() bool {
	return w.size >= 0
}
