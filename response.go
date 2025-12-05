package traefik_idempotency

import (
	"bytes"
	"net/http"
	"strings"
	"time"
)

// Record status constants.
const (
	statusProcessing = "processing"
	statusCompleted  = "completed"
)

// CachedResponse represents a stored HTTP response.
type CachedResponse struct {
	StatusCode int                 `json:"statusCode"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`       // base64 encoded for binary, plain text otherwise
	BodyIsText bool                `json:"bodyIsText"` // if true, Body is plain text
}

// RequestRecord stores the state of an idempotent request.
type RequestRecord struct {
	Fingerprint string          `json:"fingerprint"`
	Status      string          `json:"status"` // "processing" or "completed"
	Response    *CachedResponse `json:"response,omitempty"`
	CreatedAt   int64           `json:"createdAt"`
}

// isStale checks if a processing record is stale and can be overwritten.
func (r *RequestRecord) isStale(lockTimeout int64) bool {
	if r.Status != statusProcessing {
		return false
	}
	return time.Now().Unix()-r.CreatedAt > lockTimeout
}

// responseRecorder captures the response for caching.
type responseRecorder struct {
	headers       http.Header
	body          *bytes.Buffer
	statusCode    int
	headerWritten bool
}

// newResponseRecorder creates a new response recorder.
func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		headers:    make(http.Header),
		body:       &bytes.Buffer{},
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.headers
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.headerWritten {
		r.statusCode = code
		r.headerWritten = true
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.headerWritten {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(b)
}

// isTextContent checks if the content type is text-based.
func isTextContent(contentType string) bool {
	if contentType == "" {
		return true // assume text if not specified
	}
	ct := strings.ToLower(contentType)
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "javascript")
}
