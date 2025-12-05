package traefik_idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// IdempotencyMiddleware is the middleware handler.
type IdempotencyMiddleware struct {
	next        http.Handler
	name        string
	config      *Config
	redis       *redisClient
	methodsMap  map[string]bool
	lockTimeout int64
}

// New creates a new IdempotencyMiddleware plugin.
func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	// Apply defaults before validation
	config.applyDefaults()

	// Validate configuration - fail fast at startup
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	methodsMap := make(map[string]bool)
	for _, m := range config.Methods {
		methodsMap[strings.ToUpper(m)] = true
	}

	rc := newRedisClient(
		config.RedisAddress,
		config.RedisPassword,
		config.RedisDB,
		time.Duration(config.ConnectionTimeout)*time.Millisecond,
		config.PoolSize,
	)

	logInfo(name, "initialized with redis=%s, methods=%v, ttl=%ds",
		config.RedisAddress, config.Methods, config.TTLSeconds)

	return &IdempotencyMiddleware{
		next:        next,
		name:        name,
		config:      config,
		redis:       rc,
		methodsMap:  methodsMap,
		lockTimeout: int64(config.LockTimeout),
	}, nil
}

// logInfo logs an informational message to stdout.
func logInfo(name, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(os.Stdout, "[%s] INFO: %s\n", name, msg)
}

// logError logs an error message to stderr.
func logError(name, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(os.Stderr, "[%s] ERROR: %s\n", name, msg)
}

// ServeHTTP handles the HTTP request.
func (m *IdempotencyMiddleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Check if this method requires idempotency handling
	if !m.methodsMap[req.Method] {
		m.next.ServeHTTP(rw, req)
		return
	}

	// Get the idempotency key from the header
	idempotencyKey := m.extractIdempotencyKey(req)

	// If no key and not required, pass through
	if idempotencyKey == "" {
		if m.config.RequireKey {
			m.respondError(rw, http.StatusBadRequest,
				"missing-idempotency-key",
				"The Idempotency-Key header is required for this request")
			return
		}
		m.next.ServeHTTP(rw, req)
		return
	}

	// Validate the key format
	if !m.isValidKey(idempotencyKey) {
		m.respondError(rw, http.StatusBadRequest,
			"invalid-idempotency-key",
			"The Idempotency-Key header value is invalid")
		return
	}

	// Read and buffer the request body for fingerprinting
	bodyBytes, err := m.readBodyWithLimit(req)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			m.next.ServeHTTP(rw, req)
			return
		}
		m.respondError(rw, http.StatusInternalServerError,
			"body-read-error",
			"Failed to read request body")
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Generate request fingerprint and Redis key
	fingerprint := m.generateFingerprint(req, bodyBytes)
	redisKey := m.config.KeyPrefix + idempotencyKey

	// Try to acquire lock or get existing record
	record := &RequestRecord{
		Fingerprint: fingerprint,
		Status:      statusProcessing,
		CreatedAt:   time.Now().Unix(),
	}

	result, err := m.tryAcquireOrGet(redisKey, record)
	if err != nil {
		if *m.config.FailOpen {
			// Redis error - fail open (allow request through)
			logError(m.name, "redis error for key %s: %v (failing open)", idempotencyKey, err)
			m.next.ServeHTTP(rw, req)
		} else {
			// Redis error - fail closed (reject request)
			logError(m.name, "redis error for key %s: %v (failing closed)", idempotencyKey, err)
			m.respondError(rw, http.StatusServiceUnavailable,
				"storage-unavailable",
				"The idempotency storage is temporarily unavailable")
		}
		return
	}

	switch result.action {
	case actionAcquired:
		m.processAndCache(rw, req, redisKey, record)
	case actionReplay:
		m.replayResponse(rw, result.existingRecord.Response)
	case actionConflict:
		m.respondError(rw, http.StatusConflict,
			"request-in-progress",
			"A request with this Idempotency-Key is currently being processed")
	case actionFingerprintMismatch:
		m.respondError(rw, http.StatusUnprocessableEntity,
			"idempotency-key-reused",
			"The Idempotency-Key has already been used with a different request payload")
	}
}

// acquireAction represents the result of trying to acquire a lock.
type acquireAction int

const (
	actionAcquired acquireAction = iota
	actionReplay
	actionConflict
	actionFingerprintMismatch
)

type acquireResult struct {
	action         acquireAction
	existingRecord *RequestRecord
}

// tryAcquireOrGet atomically tries to acquire the lock or returns the existing record.
func (m *IdempotencyMiddleware) tryAcquireOrGet(key string, record *RequestRecord) (*acquireResult, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}

	// Try SETNX first (atomic)
	acquired, err := m.redis.setNX(key, string(data), m.config.TTLSeconds)
	if err != nil {
		return nil, err
	}

	if acquired {
		return &acquireResult{action: actionAcquired}, nil
	}

	// Key exists, get the existing record
	existingData, err := m.redis.get(key)
	if err != nil {
		if errors.Is(err, errNil) {
			// Key was deleted between SETNX and GET, retry
			acquired, err = m.redis.setNX(key, string(data), m.config.TTLSeconds)
			if err != nil {
				return nil, err
			}
			if acquired {
				return &acquireResult{action: actionAcquired}, nil
			}
			existingData, err = m.redis.get(key)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	var existingRecord RequestRecord
	if err := json.Unmarshal([]byte(existingData), &existingRecord); err != nil {
		// Corrupted data, overwrite it
		if err := m.redis.setEX(key, string(data), m.config.TTLSeconds); err != nil {
			return nil, err
		}
		return &acquireResult{action: actionAcquired}, nil
	}

	// Check fingerprint match
	if existingRecord.Fingerprint != record.Fingerprint {
		return &acquireResult{
			action:         actionFingerprintMismatch,
			existingRecord: &existingRecord,
		}, nil
	}

	// Same fingerprint - check status
	if existingRecord.Status == statusCompleted && existingRecord.Response != nil {
		return &acquireResult{
			action:         actionReplay,
			existingRecord: &existingRecord,
		}, nil
	}

	// Check if lock is stale
	if existingRecord.isStale(m.lockTimeout) {
		if err := m.redis.setEX(key, string(data), m.config.TTLSeconds); err != nil {
			return nil, err
		}
		return &acquireResult{action: actionAcquired}, nil
	}

	return &acquireResult{
		action:         actionConflict,
		existingRecord: &existingRecord,
	}, nil
}

// processAndCache processes the request and caches the response.
func (m *IdempotencyMiddleware) processAndCache(rw http.ResponseWriter, req *http.Request, redisKey string, record *RequestRecord) {
	recorder := newResponseRecorder()

	m.next.ServeHTTP(recorder, req)

	// Build cached response
	cachedResp := &CachedResponse{
		StatusCode: recorder.statusCode,
		Headers:    make(map[string][]string),
		BodyIsText: isTextContent(recorder.headers.Get("Content-Type")),
	}

	for key, values := range recorder.headers {
		cachedResp.Headers[key] = values
	}

	bodyBytes := recorder.body.Bytes()
	if cachedResp.BodyIsText {
		cachedResp.Body = string(bodyBytes)
	} else {
		cachedResp.Body = base64.StdEncoding.EncodeToString(bodyBytes)
	}

	record.Status = statusCompleted
	record.Response = cachedResp

	_ = m.storeRecord(redisKey, record)

	// Write response to client
	for key, values := range recorder.headers {
		for _, value := range values {
			rw.Header().Add(key, value)
		}
	}
	rw.WriteHeader(recorder.statusCode)
	_, _ = rw.Write(bodyBytes)
}

// extractIdempotencyKey extracts and unquotes the idempotency key from the header.
func (m *IdempotencyMiddleware) extractIdempotencyKey(req *http.Request) string {
	key := req.Header.Get(m.config.KeyHeader)
	if key == "" {
		return ""
	}

	// Handle RFC 8941 structured header format (quoted string)
	key = strings.TrimSpace(key)
	if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
		key = key[1 : len(key)-1]
	}

	return key
}

// isValidKey validates the idempotency key format.
func (m *IdempotencyMiddleware) isValidKey(key string) bool {
	if len(key) == 0 || len(key) > 256 {
		return false
	}

	for _, r := range key {
		isLower := r >= 'a' && r <= 'z'
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		isSpecial := r == '-' || r == '_'
		if !isLower && !isUpper && !isDigit && !isSpecial {
			return false
		}
	}

	return true
}

var errBodyTooLarge = errors.New("body too large")

// readBodyWithLimit reads the request body with a size limit.
func (m *IdempotencyMiddleware) readBodyWithLimit(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return []byte{}, nil
	}

	limitedReader := io.LimitReader(req.Body, m.config.MaxBodySize+1)
	bodyBytes, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, err
	}

	if int64(len(bodyBytes)) > m.config.MaxBodySize {
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(bodyBytes), req.Body))
		return nil, errBodyTooLarge
	}

	return bodyBytes, nil
}

// generateFingerprint creates a hash of the request for comparison.
func (m *IdempotencyMiddleware) generateFingerprint(req *http.Request, body []byte) string {
	h := sha256.New()
	h.Write([]byte(req.Method))
	h.Write([]byte{0})
	h.Write([]byte(req.URL.Path))
	h.Write([]byte{0})
	h.Write([]byte(req.URL.RawQuery))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// respondError sends an RFC 7807 problem details response.
func (m *IdempotencyMiddleware) respondError(rw http.ResponseWriter, status int, errType, detail string) {
	rw.Header().Set("Content-Type", "application/problem+json")
	rw.WriteHeader(status)

	problem := map[string]interface{}{
		"type":   "about:blank#" + errType,
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	}

	data, _ := json.Marshal(problem)
	_, _ = rw.Write(data)
}

// replayResponse writes a cached response to the response writer.
func (m *IdempotencyMiddleware) replayResponse(rw http.ResponseWriter, resp *CachedResponse) {
	for key, values := range resp.Headers {
		for _, value := range values {
			rw.Header().Add(key, value)
		}
	}
	rw.WriteHeader(resp.StatusCode)

	var body []byte
	if resp.BodyIsText {
		body = []byte(resp.Body)
	} else {
		var err error
		body, err = base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			body = []byte(resp.Body)
		}
	}
	_, _ = rw.Write(body)
}

// storeRecord stores a request record in Redis.
func (m *IdempotencyMiddleware) storeRecord(key string, record *RequestRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return m.redis.setEX(key, string(data), m.config.TTLSeconds)
}
