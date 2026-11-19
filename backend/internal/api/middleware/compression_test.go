package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bigJSON is a JSON body of about 2 KiB, over the default minimum.
var bigJSON = `{"data":"` + strings.Repeat("abcdefgh", 256) + `"}`

const smallJSON = `{"ok":true}`

// compressionRouter serves handler on GET, HEAD and POST /x behind
// Recovery and Compression.
func compressionRouter(minSize int, handler gin.HandlerFunc, pre ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(Recovery())
	r.Use(Compression(CompressionOptions{MinSize: minSize}))
	for _, p := range pre {
		r.Use(p)
	}
	r.GET("/x", handler)
	r.HEAD("/x", handler)
	r.POST("/x", handler)
	return r
}

func compServe(r http.Handler, method, target string, hdr map[string]string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err, "the gzip stream is complete")
	require.NoError(t, zr.Close())
	return string(out)
}

func jsonHandler(body string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(body))
	}
}

func typedHandler(status int, contentType, body string, extra map[string]string) gin.HandlerFunc {
	return func(c *gin.Context) {
		for k, v := range extra {
			c.Header(k, v)
		}
		// Not c.Data: it sets Content-Length itself.
		c.Header("Content-Type", contentType)
		c.Status(status)
		_, _ = c.Writer.WriteString(body)
	}
}

func TestCompression(t *testing.T) {
	t.Parallel()

	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00\x01\x02", 800)
	html := "<html><body>" + strings.Repeat("hello ", 400) + "</body></html>"

	tests := []struct {
		handler      gin.HandlerFunc
		reqHeaders   map[string]string
		name         string
		method       string
		wantBody     string
		wantType     string
		minSize      int
		wantGzip     bool
		wantVary     bool
		wantNoBody   bool
		wantNoLen    bool
		wantKeepLen  string
		wantEncoding string
	}{
		{name: "large json is compressed", handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "gzip among others", handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "br, gzip, deflate"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "uppercase coding with q", handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "GZIP; q=0.5"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "q=0 refuses gzip", handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip;q=0"}, wantVary: true, wantBody: bigJSON},
		{name: "q=0.000 refuses gzip", handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip; q=0.000"}, wantVary: true, wantBody: bigJSON},
		{name: "star is not gzip", handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "*"}, wantVary: true, wantBody: bigJSON},
		{name: "identity only", handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "identity"}, wantVary: true, wantBody: bigJSON},
		{name: "no Accept-Encoding", handler: jsonHandler(bigJSON), wantVary: true, wantBody: bigJSON},
		{name: "HEAD is never compressed", method: http.MethodHead, handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true, wantBody: bigJSON},
		{name: "small body is sent as it was", handler: jsonHandler(smallJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true, wantBody: smallJSON},
		{name: "body one byte under the minimum", minSize: len(bigJSON) + 1, handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true, wantBody: bigJSON},
		{name: "body exactly the minimum", minSize: len(bigJSON), handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "204 has no body", handler: func(c *gin.Context) { c.Status(http.StatusNoContent) }, reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantNoBody: true},
		{name: "304 has no body", handler: func(c *gin.Context) { c.Header("Content-Type", "application/json"); c.Status(http.StatusNotModified) }, reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantNoBody: true},
		{name: "existing Content-Encoding", handler: typedHandler(http.StatusOK, "application/json", bigJSON, map[string]string{"Content-Encoding": "br"}), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true, wantBody: bigJSON, wantEncoding: "br"},
		{name: "206 partial content", handler: typedHandler(http.StatusPartialContent, "application/json", bigJSON, map[string]string{"Content-Range": "bytes 0-9/100"}), reqHeaders: map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9"}, wantVary: true, wantBody: bigJSON},
		{name: "Content-Range on a 200", handler: typedHandler(http.StatusOK, "application/json", bigJSON, map[string]string{"Content-Range": "bytes 0-9/100"}), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true, wantBody: bigJSON},
		{name: "no-transform", handler: typedHandler(http.StatusOK, "application/json", bigJSON, map[string]string{"Cache-Control": "public, no-transform"}), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true, wantBody: bigJSON},
		{name: "png is not compressible", handler: typedHandler(http.StatusOK, "image/png", png, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantBody: png},
		{name: "octet-stream is not compressible", handler: typedHandler(http.StatusOK, "application/octet-stream", bigJSON, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantBody: bigJSON},
		{name: "text html", handler: typedHandler(http.StatusOK, "text/html; charset=utf-8", html, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: html},
		{name: "problem+json", handler: typedHandler(http.StatusOK, "application/problem+json", bigJSON, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "xml", handler: typedHandler(http.StatusOK, "application/xml", bigJSON, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "atom+xml", handler: typedHandler(http.StatusOK, "application/atom+xml", bigJSON, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "javascript", handler: typedHandler(http.StatusOK, "application/javascript", bigJSON, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "svg", handler: typedHandler(http.StatusOK, "image/svg+xml", bigJSON, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "invalid content type", handler: typedHandler(http.StatusOK, "///", bigJSON, nil), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantBody: bigJSON},
		{name: "declared length over the minimum", handler: typedHandler(http.StatusOK, "application/json", bigJSON, map[string]string{"Content-Length": strconv.Itoa(len(bigJSON))}), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantNoLen: true, wantBody: bigJSON},
		{name: "declared length under the minimum", handler: typedHandler(http.StatusOK, "application/json", smallJSON, map[string]string{"Content-Length": strconv.Itoa(len(smallJSON))}), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true, wantKeepLen: strconv.Itoa(len(smallJSON)), wantBody: smallJSON},
		{name: "declared length that is not a number", handler: typedHandler(http.StatusOK, "application/json", bigJSON, map[string]string{"Content-Length": "many"}), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantVary: true},
		{name: "no content type, text body", handler: func(c *gin.Context) { _, _ = c.Writer.Write([]byte(bigJSON)) }, reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantType: "text/plain; charset=utf-8", wantBody: bigJSON},
		{name: "no content type, binary body", handler: func(c *gin.Context) { _, _ = c.Writer.Write([]byte(png)) }, reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantBody: png},
		{name: "no content type, no body", handler: func(c *gin.Context) { c.Status(http.StatusAccepted) }, reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantNoBody: true},
		{name: "several writes cross the minimum", handler: func(c *gin.Context) {
			c.Header("Content-Type", "application/json")
			for i := 0; i < 8; i++ {
				_, _ = c.Writer.WriteString(bigJSON[i*250 : (i+1)*250])
			}
			_, _ = c.Writer.WriteString(bigJSON[2000:])
		}, reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
		{name: "POST response is compressed", method: http.MethodPost, handler: jsonHandler(bigJSON), reqHeaders: map[string]string{"Accept-Encoding": "gzip"}, wantGzip: true, wantVary: true, wantBody: bigJSON},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			minSize := tt.minSize
			if minSize == 0 {
				minSize = 1024
			}
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			w := compServe(compressionRouter(minSize, tt.handler), method, "/x", tt.reqHeaders, nil)

			assert.Equal(t, tt.wantVary, strings.Contains(w.Header().Get("Vary"), "Accept-Encoding"), "Vary: %q", w.Header().Get("Vary"))
			if tt.wantGzip {
				assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
				assert.Empty(t, w.Header().Get("Content-Length"))
				if tt.wantBody != "" {
					assert.Equal(t, tt.wantBody, gunzip(t, w.Body.Bytes()))
				}
				if tt.wantType != "" {
					assert.Equal(t, tt.wantType, w.Header().Get("Content-Type"))
				}
				assert.Less(t, w.Body.Len(), len(tt.wantBody), "the body got smaller")
				return
			}
			assert.Equal(t, tt.wantEncoding, w.Header().Get("Content-Encoding"), "not compressed")
			if tt.wantNoBody {
				assert.Zero(t, w.Body.Len())
			} else if tt.wantBody != "" {
				assert.Equal(t, tt.wantBody, w.Body.String())
			}
			if tt.wantKeepLen != "" {
				assert.Equal(t, tt.wantKeepLen, w.Header().Get("Content-Length"))
			}
		})
	}
}

func TestCompressionETag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, etag, want string
		gzip             bool
	}{
		{name: "strong becomes weak", etag: `"abc"`, want: `W/"abc"`, gzip: true},
		{name: "weak stays", etag: `W/"abc"`, want: `W/"abc"`, gzip: true},
		{name: "uncompressed keeps a strong tag", etag: `"abc"`, want: `"abc"`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := bigJSON
			if !tt.gzip {
				body = smallJSON
			}
			r := compressionRouter(1024, typedHandler(http.StatusOK, "application/json", body, map[string]string{"ETag": tt.etag}))
			w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
			assert.Equal(t, tt.want, w.Header().Get("ETag"))
		})
	}
}

func TestCompressionKeepsExistingVary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, existing string
		wantValues     int
	}{
		{name: "adds to another value", existing: "Origin", wantValues: 2},
		{name: "does not repeat", existing: "Origin, accept-encoding", wantValues: 1},
		{name: "star covers it", existing: "*", wantValues: 1},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := compressionRouter(1024, typedHandler(http.StatusOK, "application/json", bigJSON, map[string]string{"Vary": tt.existing}))
			w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
			assert.Len(t, w.Header().Values("Vary"), tt.wantValues)
			assert.Contains(t, strings.ToLower(strings.Join(w.Header().Values("Vary"), ",")), strings.ToLower(tt.existing))
		})
	}
}

func TestCompressionHandlerStatusAndHeadersSurvive(t *testing.T) {
	t.Parallel()
	r := compressionRouter(1024, func(c *gin.Context) {
		c.Header("X-Custom", "yes")
		c.JSON(http.StatusCreated, gin.H{"data": strings.Repeat("z", 3000)})
	})
	w := compServe(r, http.MethodPost, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "yes", w.Header().Get("X-Custom"))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.Contains(t, gunzip(t, w.Body.Bytes()), strings.Repeat("z", 3000))
}

func TestCompressionStatusAfterWriteIsIgnored(t *testing.T) {
	t.Parallel()
	// Like gin without the middleware: a status set after the first write
	// does not change the response.
	r := compressionRouter(1024, func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		_, _ = c.Writer.WriteString(smallJSON)
		c.Writer.WriteHeader(http.StatusTeapot)
	})
	w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, smallJSON, w.Body.String())
}

func TestCompressionWriterKeepsGinSemantics(t *testing.T) {
	t.Parallel()
	type seen struct {
		status, size         int
		written, writtenLate bool
		sizeLate, statusLate int
	}
	var got seen
	r := compressionRouter(1024, func(c *gin.Context) {
		got.written = c.Writer.Written()
		got.size = c.Writer.Size()
		c.Writer.WriteHeader(http.StatusAccepted)
		got.status = c.Writer.Status()
		_, _ = c.Writer.WriteString(bigJSON)
		got.writtenLate = c.Writer.Written()
		got.sizeLate = c.Writer.Size()
		got.statusLate = c.Writer.Status()
		c.Header("Content-Type", "application/json")
	})
	w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.False(t, got.written)
	assert.Equal(t, -1, got.size)
	assert.Equal(t, http.StatusAccepted, got.status)
	assert.True(t, got.writtenLate)
	assert.Equal(t, len(bigJSON), got.sizeLate, "Size counts what the handler wrote, not the compressed bytes")
	assert.Equal(t, http.StatusAccepted, got.statusLate)
}

func TestCompressionLeavesAnUnwrittenResponseToGin(t *testing.T) {
	t.Parallel()
	// A chain that writes nothing must not commit the response: Gin writes
	// its own 404 body after the middleware returned.
	r := gin.New()
	r.Use(Compression(CompressionOptions{}))
	w := compServe(r, http.MethodGet, "/missing", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "404 page not found", w.Body.String())
}

func TestCompressionAbortWithStatusIsPassedOn(t *testing.T) {
	t.Parallel()
	r := gin.New()
	r.Use(Compression(CompressionOptions{}), func(c *gin.Context) { c.AbortWithStatus(http.StatusForbidden) })
	r.GET("/x", func(c *gin.Context) { c.String(200, "no") })
	w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Zero(t, w.Body.Len())
}

func TestCompressionDefaultMinSize(t *testing.T) {
	t.Parallel()
	r := compressionRouter(0, jsonHandler(strings.Repeat("a", DefaultCompressionMinSize-1)))
	w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Empty(t, w.Header().Get("Content-Encoding"))

	r = compressionRouter(0, jsonHandler(strings.Repeat("a", DefaultCompressionMinSize)))
	w = compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
}

func TestCompressionFlush(t *testing.T) {
	t.Parallel()

	t.Run("flush after a small write starts a stream", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1024, func(c *gin.Context) {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString("data: one\n\n")
			c.Writer.Flush()
			_, _ = c.Writer.WriteString("data: two\n\n")
		})
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.True(t, w.Flushed)
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, "data: one\n\ndata: two\n\n", gunzip(t, w.Body.Bytes()))
	})

	t.Run("flush with no data sends the headers", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1024, func(c *gin.Context) {
			c.Header("Content-Type", "text/event-stream")
			c.Writer.Flush()
			_, _ = c.Writer.WriteString("data: late\n\n")
		})
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, "data: late\n\n", gunzip(t, w.Body.Bytes()))
	})

	t.Run("flush of an uncompressed response", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1024, func(c *gin.Context) {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString("data: one\n\n")
			c.Writer.Flush()
		})
		w := compServe(r, http.MethodGet, "/x", nil, nil)
		assert.True(t, w.Flushed)
		assert.Empty(t, w.Header().Get("Content-Encoding"))
		assert.Equal(t, "data: one\n\n", w.Body.String())
	})

	t.Run("flush before anything is known", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1024, func(c *gin.Context) {
			c.Writer.Flush()
		})
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.True(t, w.Flushed)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// TestCompressionStreamsOverARealConnection reads the first event of a
// compressed stream before the handler has written the second.
func TestCompressionStreamsOverARealConnection(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	r := compressionRouter(1024, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: one\n\n")
		c.Writer.Flush()
		select {
		case <-release:
		case <-c.Request.Context().Done():
		}
		_, _ = c.Writer.WriteString("data: two\n\n")
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))

	zr, err := gzip.NewReader(resp.Body)
	require.NoError(t, err)
	first := make([]byte, len("data: one\n\n"))
	_, err = io.ReadFull(zr, first)
	require.NoError(t, err)
	assert.Equal(t, "data: one\n\n", string(first))

	close(release)
	rest, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, "data: two\n\n", string(rest))
}

func TestCompressionPanic(t *testing.T) {
	t.Parallel()

	t.Run("panic before any write answers the plain 500", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1, func(c *gin.Context) {
			c.Header("Content-Type", "application/json")
			panic("boom")
		})
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
		assert.NotContains(t, w.Body.String(), "boom")
	})

	t.Run("panic with a buffered body drops it", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1024, func(c *gin.Context) {
			c.Header("Content-Type", "application/json")
			_, _ = c.Writer.WriteString(`{"partial":`)
			panic("boom")
		})
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
	})

	t.Run("panic after the stream started leaves a truncated gzip stream", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1024, func(c *gin.Context) {
			c.Header("Content-Type", "application/json")
			_, _ = c.Writer.WriteString(bigJSON)
			c.Writer.Flush()
			panic("boom")
		})
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, http.StatusOK, w.Code, "the status was already sent")
		zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		require.NoError(t, err)
		out, err := io.ReadAll(zr)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF, "the client sees a cut stream, not a complete response")
		assert.Equal(t, bigJSON, string(out), "what was flushed before the panic arrives")
		assert.NotContains(t, w.Body.String(), "Internal server error", "the 500 body is not appended")
	})

	t.Run("panic after the stream started, nothing flushed", func(t *testing.T) {
		t.Parallel()
		r := compressionRouter(1024, func(c *gin.Context) {
			c.Header("Content-Type", "application/json")
			_, _ = c.Writer.WriteString(bigJSON)
			panic("boom")
		})
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err == nil {
			_, err = io.ReadAll(zr)
		}
		require.Error(t, err, "the stream has no trailer")
		assert.NotContains(t, w.Body.String(), "Internal server error")
	})

	t.Run("the writer is restored for the middleware outside", func(t *testing.T) {
		t.Parallel()
		var after gin.ResponseWriter
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Next()
			after = c.Writer
		})
		r.Use(Recovery(), Compression(CompressionOptions{MinSize: 10}))
		r.GET("/x", func(c *gin.Context) { panic("boom") })
		w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		_, wrapped := after.(*compressWriter)
		assert.False(t, wrapped)
		assert.Equal(t, http.StatusInternalServerError, after.Status(), "the Logger and the metrics see the status")
	})
}

func TestCompressionOuterMiddlewareSeesTheStatus(t *testing.T) {
	t.Parallel()
	var status, size int
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Next()
		status = c.Writer.Status()
		size = c.Writer.Size()
	})
	r.Use(Compression(CompressionOptions{}))
	r.GET("/x", func(c *gin.Context) { c.JSON(http.StatusCreated, gin.H{"d": strings.Repeat("q", 4000)}) })
	w := compServe(r, http.MethodGet, "/x", map[string]string{"Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, http.StatusCreated, status)
	assert.Equal(t, w.Body.Len(), size, "the outer writer reports the bytes that were sent")
	assert.Less(t, size, 4000)
}

func TestCompressionConcurrentRequestsShareThePool(t *testing.T) {
	t.Parallel()
	r := compressionRouter(1024, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.Query("id"), "pad": strings.Repeat("p", 2000)})
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := strconv.Itoa(i)
			w := compServe(r, http.MethodGet, "/x?id="+id, map[string]string{"Accept-Encoding": "gzip"}, nil)
			zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
			if !assert.NoError(t, err) {
				return
			}
			out, err := io.ReadAll(zr)
			assert.NoError(t, err)
			assert.Contains(t, string(out), `"id":"`+id+`"`)
		}(i)
	}
	wg.Wait()
}

func TestCompressionSkipsWebSocketRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		hdr  map[string]string
		name string
		path string
		skip bool
	}{
		{name: "the /ws path", path: "/ws", skip: true},
		{name: "an upgrade header", path: "/other", hdr: map[string]string{"Upgrade": "websocket"}, skip: true},
		{name: "an upgrade header in a list", path: "/other", hdr: map[string]string{"Upgrade": "h2c, WebSocket"}, skip: true},
		{name: "a normal path", path: "/other"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var wrapped bool
			r := gin.New()
			r.Use(Compression(CompressionOptions{}))
			h := func(c *gin.Context) {
				_, wrapped = c.Writer.(*compressWriter)
				c.Status(http.StatusOK)
			}
			r.GET("/ws", h)
			r.GET("/other", h)
			hdr := map[string]string{"Accept-Encoding": "gzip"}
			for k, v := range tt.hdr {
				hdr[k] = v
			}
			compServe(r, http.MethodGet, tt.path, hdr, nil)
			assert.Equal(t, !tt.skip, wrapped)
		})
	}
}

func TestCompressionNeverTouchesTheRequestBody(t *testing.T) {
	t.Parallel()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"name":"x"}`))
	require.NoError(t, zw.Close())
	raw := gz.Bytes()

	var got []byte
	r := gin.New()
	r.Use(Compression(CompressionOptions{}))
	r.POST("/x", func(c *gin.Context) {
		got, _ = io.ReadAll(c.Request.Body)
		c.Status(http.StatusNoContent)
	})
	w := compServe(r, http.MethodPost, "/x", map[string]string{"Content-Encoding": "gzip", "Accept-Encoding": "gzip"}, bytes.NewReader(raw))
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, raw, got, "a request body is not decompressed")
}

func TestCompressionOversizedRequestIs413(t *testing.T) {
	t.Parallel()
	r := gin.New()
	r.Use(Recovery(), Compression(CompressionOptions{}), MaxBodySize(1<<20))
	r.POST("/x", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
			return
		}
		c.Status(http.StatusOK)
	})

	t.Run("declared length", func(t *testing.T) {
		t.Parallel()
		w := compServe(r, http.MethodPost, "/x", map[string]string{"Accept-Encoding": "gzip"}, strings.NewReader(strings.Repeat("a", 2<<20)))
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	})
	t.Run("chunked", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", 2<<20)))
		req.ContentLength = -1
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	})
}

func TestCompressionAcceptsGzip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "none"},
		{name: "empty", values: []string{""}},
		{name: "gzip", values: []string{"gzip"}, want: true},
		{name: "spaces", values: []string{" gzip , br "}, want: true},
		{name: "q one", values: []string{"gzip;q=1.0"}, want: true},
		{name: "q small", values: []string{"gzip;q=0.001"}, want: true},
		{name: "q zero", values: []string{"gzip;q=0"}},
		{name: "q zero uppercase", values: []string{"gzip;Q=0.0"}},
		{name: "q invalid", values: []string{"gzip;q=abc"}},
		{name: "q negative", values: []string{"gzip;q=-1"}},
		{name: "q one with zeros", values: []string{"gzip;q=1.000"}, want: true},
		{name: "q zero with a dot", values: []string{"gzip;q=0."}},
		{name: "q zero with three digits", values: []string{"gzip;q=0.500"}, want: true},
		{name: "q over one", values: []string{"gzip;q=1.5"}},
		{name: "q one with a digit", values: []string{"gzip;q=1.001"}},
		{name: "q four decimals", values: []string{"gzip;q=0.5000"}},
		{name: "q two", values: []string{"gzip;q=2"}},
		{name: "q NaN", values: []string{"gzip;q=NaN"}},
		{name: "q infinity", values: []string{"gzip;q=Inf"}},
		{name: "q exponent", values: []string{"gzip;q=1e0"}},
		{name: "q hex float", values: []string{"gzip;q=0x1p-1"}},
		{name: "q plus sign", values: []string{"gzip;q=+1"}},
		{name: "q leading dot", values: []string{"gzip;q=.5"}},
		{name: "q empty", values: []string{"gzip;q="}},
		{name: "q with a space after the sign", values: []string{"gzip;q= 1"}},
		{name: "q underscore digits", values: []string{"gzip;q=0_5"}},
		{name: "other parameter", values: []string{"gzip;level=1;q=0.4"}, want: true},
		{name: "star", values: []string{"*"}},
		{name: "star with gzip refused", values: []string{"*, gzip;q=0"}},
		{name: "x-gzip is not handled", values: []string{"x-gzip"}},
		{name: "second header line", values: []string{"br", "gzip"}, want: true},
		{name: "refusal wins over an offer", values: []string{"gzip, gzip;q=0"}},
		{name: "gzipx", values: []string{"gzipx"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, acceptsGzip(tt.values))
		})
	}
}

func TestCompressionCompressibleType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"Application/JSON; charset=utf-8", true},
		{"application/problem+json", true},
		{"application/ld+json", true},
		{"text/plain", true},
		{"text/html; charset=utf-8", true},
		{"text/event-stream", true},
		{"application/xml", true},
		{"application/rss+xml", true},
		{"application/javascript", true},
		{"image/svg+xml", true},
		{"image/png", false},
		{"image/jpeg", false},
		{"application/octet-stream", false},
		{"application/zip", false},
		{"application/gzip", false},
		{"video/mp4", false},
		{"", false},
		{"nonsense", false},
		{"application/jsonx", false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.ct, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, compressibleType(tt.ct))
		})
	}
}

// newCompressedIdemRouter stacks Recovery, Compression and Idempotency (as
// routes.Setup does with auth off) over a handler that counts its runs.
func newCompressedIdemRouter(store *fakeIdemStore, runs *int, mu *sync.Mutex) *gin.Engine {
	r := gin.New()
	r.Use(Recovery(), Compression(CompressionOptions{MinSize: 1024}))
	r.Use(Idempotency(IdempotencyOptions{Store: store, TTL: time.Hour, LockTimeout: time.Minute}))
	h := func(c *gin.Context) {
		mu.Lock()
		*runs++
		mu.Unlock()
		c.Data(http.StatusCreated, "application/json; charset=utf-8", []byte(bigJSON))
	}
	r.POST("/x", h)
	r.GET("/x", h)
	r.HEAD("/x", h)
	return r
}

func TestCompressionWithIdempotency(t *testing.T) {
	t.Parallel()
	store := newFakeIdemStore()
	var (
		runs int
		mu   sync.Mutex
	)
	r := newCompressedIdemRouter(store, &runs, &mu)
	hdr := func(enc string) map[string]string {
		h := map[string]string{IdempotencyKeyHeader: "k1", "Content-Type": "application/json"}
		if enc != "" {
			h["Accept-Encoding"] = enc
		}
		return h
	}

	first := compServe(r, http.MethodPost, "/x", hdr("gzip"), strings.NewReader(`{"a":1}`))
	require.Equal(t, http.StatusCreated, first.Code)
	assert.Equal(t, "gzip", first.Header().Get("Content-Encoding"))
	assert.Equal(t, bigJSON, gunzip(t, first.Body.Bytes()))

	// The store holds the uncompressed response.
	require.Len(t, store.rows, 1)
	for _, row := range store.rows {
		assert.Equal(t, bigJSON, string(row.resp.Body))
		assert.Equal(t, "application/json; charset=utf-8", row.resp.ContentType)
	}

	plain := compServe(r, http.MethodPost, "/x", hdr(""), strings.NewReader(`{"a":1}`))
	assert.Equal(t, "true", plain.Header().Get(IdempotencyReplayedHeader))
	assert.Empty(t, plain.Header().Get("Content-Encoding"), "the replay follows the Accept-Encoding of the replay request")
	assert.Equal(t, bigJSON, plain.Body.String())
	assert.Contains(t, plain.Header().Get("Vary"), "Accept-Encoding")

	again := compServe(r, http.MethodPost, "/x", hdr("gzip"), strings.NewReader(`{"a":1}`))
	assert.Equal(t, "true", again.Header().Get(IdempotencyReplayedHeader))
	assert.Equal(t, "gzip", again.Header().Get("Content-Encoding"))
	assert.Equal(t, bigJSON, gunzip(t, again.Body.Bytes()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, runs, "the handler ran once")
}

func TestCompressionHeadWithIdempotencyKey(t *testing.T) {
	t.Parallel()
	store := newFakeIdemStore()
	var (
		runs int
		mu   sync.Mutex
	)
	r := newCompressedIdemRouter(store, &runs, &mu)
	w := compServe(r, http.MethodHead, "/x", map[string]string{IdempotencyKeyHeader: "k", "Accept-Encoding": "gzip"}, nil)
	assert.Equal(t, http.StatusCreated, w.Code)
	// The recorder keeps the body that a real server drops for HEAD.
	assert.Equal(t, bigJSON, w.Body.String(), "not compressed")
	assert.Empty(t, w.Header().Get("Content-Encoding"))
	assert.Empty(t, w.Header().Get(IdempotencyReplayedHeader))
	assert.Empty(t, store.rows, "only POST is stored")
}

func TestCompressionWithIdempotencyAuthOffPanicReleasesKey(t *testing.T) {
	t.Parallel()
	store := newFakeIdemStore()
	r := gin.New()
	r.Use(Recovery(), Compression(CompressionOptions{MinSize: 1024}))
	r.Use(Idempotency(IdempotencyOptions{Store: store, TTL: time.Hour, LockTimeout: time.Minute}))
	r.POST("/x", func(c *gin.Context) { panic("boom") })
	w := compServe(r, http.MethodPost, "/x", map[string]string{IdempotencyKeyHeader: "k", "Accept-Encoding": "gzip"}, strings.NewReader(`{}`))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
	assert.Equal(t, 1, store.releases)
}

var _ models.IdempotencyRepository = (*fakeIdemStore)(nil)

func TestCompressionAbandonGzipReturnsTheWriter(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	cw := &compressWriter{ResponseWriter: c.Writer, size: -1}
	require.NoError(t, cw.startGzip())
	require.NotNil(t, cw.gz)
	_, err := cw.gz.Write([]byte(bigJSON))
	require.NoError(t, err)

	cw.abandonGzip()
	assert.Nil(t, cw.gz, "the writer went back to the pool")
	assert.True(t, cw.dead)
	before := rec.Body.Len()
	_, err = cw.Write([]byte("later"))
	require.NoError(t, err)
	cw.Flush()
	cw.abandonGzip() // idempotent
	assert.Equal(t, before, rec.Body.Len(), "nothing is written after the abandon")
}
