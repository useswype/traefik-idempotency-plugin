package traefik_idempotency

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockHandler is a simple test handler.
type mockHandler struct {
	called     int
	statusCode int
	response   string
	headers    map[string]string
}

func (h *mockHandler) ServeHTTP(rw http.ResponseWriter, _ *http.Request) {
	h.called++
	for k, v := range h.headers {
		rw.Header().Set(k, v)
	}
	rw.WriteHeader(h.statusCode)
	_, _ = rw.Write([]byte(h.response))
}

func TestCreateConfig(t *testing.T) {
	config := CreateConfig()

	if config.RedisAddress != "localhost:6379" {
		t.Errorf("expected default RedisAddress 'localhost:6379', got '%s'", config.RedisAddress)
	}

	if config.KeyHeader != "Idempotency-Key" {
		t.Errorf("expected default KeyHeader 'Idempotency-Key', got '%s'", config.KeyHeader)
	}

	if config.TTLSeconds != 86400 {
		t.Errorf("expected default TTLSeconds 86400, got %d", config.TTLSeconds)
	}

	if config.RequireKey {
		t.Error("expected default RequireKey false, got true")
	}

	if len(config.Methods) != 2 || config.Methods[0] != "POST" || config.Methods[1] != "PATCH" {
		t.Errorf("expected default Methods ['POST', 'PATCH'], got %v", config.Methods)
	}

	if config.MaxBodySize != defaultMaxBodySize {
		t.Errorf("expected default MaxBodySize %d, got %d", defaultMaxBodySize, config.MaxBodySize)
	}

	if config.LockTimeout != 60 {
		t.Errorf("expected default LockTimeout 60, got %d", config.LockTimeout)
	}

	if config.FailOpen == nil || !*config.FailOpen {
		t.Error("expected default FailOpen true")
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		expectError bool
	}{
		{
			name: "valid config",
			config: &Config{
				RedisAddress: "localhost:6379",
			},
			expectError: false,
		},
		{
			name: "missing redis address",
			config: &Config{
				RedisAddress: "",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &mockHandler{}
			_, err := New(context.Background(), handler, tt.config, "test")
			if tt.expectError && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestExtractIdempotencyKey(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
		KeyHeader:    "Idempotency-Key",
	}

	handler := &mockHandler{}
	mw, _ := New(context.Background(), handler, config, "test")
	middleware, ok := mw.(*IdempotencyMiddleware)
	if !ok {
		t.Fatal("failed to cast to IdempotencyMiddleware")
	}

	tests := []struct {
		name     string
		header   string
		expected string
	}{
		{
			name:     "quoted string per RFC 8941",
			header:   `"8e03978e-40d5-43e8-bc93-6894a57f9324"`,
			expected: "8e03978e-40d5-43e8-bc93-6894a57f9324",
		},
		{
			name:     "unquoted string",
			header:   "8e03978e-40d5-43e8-bc93-6894a57f9324",
			expected: "8e03978e-40d5-43e8-bc93-6894a57f9324",
		},
		{
			name:     "empty header",
			header:   "",
			expected: "",
		},
		{
			name:     "whitespace padded",
			header:   `  "test-key-123"  `,
			expected: "test-key-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.header != "" {
				req.Header.Set("Idempotency-Key", tt.header)
			}
			result := middleware.extractIdempotencyKey(req)
			if result != tt.expected {
				t.Errorf("expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestIsValidKey(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
	}

	handler := &mockHandler{}
	mw, _ := New(context.Background(), handler, config, "test")
	middleware, ok := mw.(*IdempotencyMiddleware)
	if !ok {
		t.Fatal("failed to cast to IdempotencyMiddleware")
	}

	tests := []struct {
		name     string
		key      string
		expected bool
	}{
		{
			name:     "valid UUID",
			key:      "8e03978e-40d5-43e8-bc93-6894a57f9324",
			expected: true,
		},
		{
			name:     "valid alphanumeric",
			key:      "abc123XYZ",
			expected: true,
		},
		{
			name:     "valid with underscores",
			key:      "test_key_123",
			expected: true,
		},
		{
			name:     "empty string",
			key:      "",
			expected: false,
		},
		{
			name:     "contains spaces",
			key:      "test key",
			expected: false,
		},
		{
			name:     "contains special chars",
			key:      "test@key!",
			expected: false,
		},
		{
			name:     "too long",
			key:      strings.Repeat("a", 300),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := middleware.isValidKey(tt.key)
			if result != tt.expected {
				t.Errorf("expected %v for key '%s', got %v", tt.expected, tt.key, result)
			}
		})
	}
}

func TestGenerateFingerprint(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
	}

	handler := &mockHandler{}
	mw, _ := New(context.Background(), handler, config, "test")
	middleware, ok := mw.(*IdempotencyMiddleware)
	if !ok {
		t.Fatal("failed to cast to IdempotencyMiddleware")
	}

	req1 := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader([]byte(`{"amount":100}`)))
	req2 := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader([]byte(`{"amount":100}`)))
	req3 := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader([]byte(`{"amount":200}`)))
	req4 := httptest.NewRequest(http.MethodPost, "/api/payments", bytes.NewReader([]byte(`{"amount":100}`)))

	body1 := []byte(`{"amount":100}`)
	body2 := []byte(`{"amount":100}`)
	body3 := []byte(`{"amount":200}`)
	body4 := []byte(`{"amount":100}`)

	fp1 := middleware.generateFingerprint(req1, body1)
	fp2 := middleware.generateFingerprint(req2, body2)
	fp3 := middleware.generateFingerprint(req3, body3)
	fp4 := middleware.generateFingerprint(req4, body4)

	// Same request should produce same fingerprint
	if fp1 != fp2 {
		t.Error("identical requests should have same fingerprint")
	}

	// Different body should produce different fingerprint
	if fp1 == fp3 {
		t.Error("different bodies should have different fingerprints")
	}

	// Different path should produce different fingerprint
	if fp1 == fp4 {
		t.Error("different paths should have different fingerprints")
	}
}

func TestRespondError(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
	}

	handler := &mockHandler{}
	mw, _ := New(context.Background(), handler, config, "test")
	middleware, ok := mw.(*IdempotencyMiddleware)
	if !ok {
		t.Fatal("failed to cast to IdempotencyMiddleware")
	}

	tests := []struct {
		name       string
		status     int
		errType    string
		detail     string
		expectType string
	}{
		{
			name:       "400 missing key",
			status:     http.StatusBadRequest,
			errType:    "missing-idempotency-key",
			detail:     "The Idempotency-Key header is required",
			expectType: "about:blank#missing-idempotency-key",
		},
		{
			name:       "409 conflict",
			status:     http.StatusConflict,
			errType:    "request-in-progress",
			detail:     "Request is being processed",
			expectType: "about:blank#request-in-progress",
		},
		{
			name:       "422 key reused",
			status:     http.StatusUnprocessableEntity,
			errType:    "idempotency-key-reused",
			detail:     "Key reused with different payload",
			expectType: "about:blank#idempotency-key-reused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rw := httptest.NewRecorder()
			middleware.respondError(rw, tt.status, tt.errType, tt.detail)

			if rw.Code != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, rw.Code)
			}

			contentType := rw.Header().Get("Content-Type")
			if contentType != "application/problem+json" {
				t.Errorf("expected Content-Type 'application/problem+json', got '%s'", contentType)
			}

			var problem map[string]interface{}
			if err := json.Unmarshal(rw.Body.Bytes(), &problem); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}

			if problem["type"] != tt.expectType {
				t.Errorf("expected type '%s', got '%v'", tt.expectType, problem["type"])
			}

			if problem["detail"] != tt.detail {
				t.Errorf("expected detail '%s', got '%v'", tt.detail, problem["detail"])
			}
		})
	}
}

func TestReplayResponse_TextContent(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
	}

	handler := &mockHandler{}
	mw, _ := New(context.Background(), handler, config, "test")
	middleware, ok := mw.(*IdempotencyMiddleware)
	if !ok {
		t.Fatal("failed to cast to IdempotencyMiddleware")
	}

	cachedResp := &CachedResponse{
		StatusCode: http.StatusCreated,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"X-Custom":     {"value1", "value2"},
		},
		Body:       `{"id":"12345"}`,
		BodyIsText: true,
	}

	rw := httptest.NewRecorder()
	middleware.replayResponse(rw, cachedResp)

	if rw.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, rw.Code)
	}

	if rw.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", rw.Header().Get("Content-Type"))
	}

	// Check multi-value header
	customValues := rw.Header().Values("X-Custom")
	if len(customValues) != 2 {
		t.Errorf("expected 2 X-Custom values, got %d", len(customValues))
	}

	if rw.Body.String() != `{"id":"12345"}` {
		t.Errorf("expected body '%s', got '%s'", `{"id":"12345"}`, rw.Body.String())
	}
}

func TestReplayResponse_BinaryContent(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
	}

	handler := &mockHandler{}
	mw, _ := New(context.Background(), handler, config, "test")
	middleware, ok := mw.(*IdempotencyMiddleware)
	if !ok {
		t.Fatal("failed to cast to IdempotencyMiddleware")
	}

	binaryData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}
	cachedResp := &CachedResponse{
		StatusCode: http.StatusOK,
		Headers: map[string][]string{
			"Content-Type": {"application/octet-stream"},
		},
		Body:       base64.StdEncoding.EncodeToString(binaryData),
		BodyIsText: false,
	}

	rw := httptest.NewRecorder()
	middleware.replayResponse(rw, cachedResp)

	if rw.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rw.Code)
	}

	if !bytes.Equal(rw.Body.Bytes(), binaryData) {
		t.Errorf("binary body mismatch: expected %v, got %v", binaryData, rw.Body.Bytes())
	}
}

func TestServeHTTP_NonTargetMethod(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
		Methods:      []string{"POST"},
	}

	handler := &mockHandler{
		statusCode: http.StatusOK,
		response:   "OK",
	}

	mw, _ := New(context.Background(), handler, config, "test")

	// GET request should pass through without idempotency processing
	req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
	rw := httptest.NewRecorder()

	mw.ServeHTTP(rw, req)

	if handler.called != 1 {
		t.Errorf("expected handler to be called once, got %d", handler.called)
	}

	if rw.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rw.Code)
	}
}

func TestServeHTTP_NoKeyNotRequired(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
		Methods:      []string{"POST"},
		RequireKey:   false,
	}

	handler := &mockHandler{
		statusCode: http.StatusCreated,
		response:   `{"id":"123"}`,
	}

	mw, _ := New(context.Background(), handler, config, "test")

	// POST without idempotency key when not required
	req := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader([]byte(`{}`)))
	rw := httptest.NewRecorder()

	mw.ServeHTTP(rw, req)

	if handler.called != 1 {
		t.Errorf("expected handler to be called once, got %d", handler.called)
	}

	if rw.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, rw.Code)
	}
}

func TestServeHTTP_NoKeyRequired(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
		Methods:      []string{"POST"},
		RequireKey:   true,
	}

	handler := &mockHandler{
		statusCode: http.StatusCreated,
		response:   `{"id":"123"}`,
	}

	mw, _ := New(context.Background(), handler, config, "test")

	// POST without idempotency key when required
	req := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader([]byte(`{}`)))
	rw := httptest.NewRecorder()

	mw.ServeHTTP(rw, req)

	if handler.called != 0 {
		t.Errorf("expected handler not to be called, got %d", handler.called)
	}

	if rw.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, rw.Code)
	}
}

func TestServeHTTP_InvalidKey(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
		Methods:      []string{"POST"},
	}

	handler := &mockHandler{
		statusCode: http.StatusCreated,
		response:   `{"id":"123"}`,
	}

	mw, _ := New(context.Background(), handler, config, "test")

	// POST with invalid idempotency key
	req := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Idempotency-Key", "invalid key with spaces!")
	rw := httptest.NewRecorder()

	mw.ServeHTTP(rw, req)

	if handler.called != 0 {
		t.Errorf("expected handler not to be called, got %d", handler.called)
	}

	if rw.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, rw.Code)
	}
}

func TestResponseRecorder(t *testing.T) {
	recorder := &responseRecorder{
		statusCode: http.StatusOK,
		headers:    make(http.Header),
		body:       &bytes.Buffer{},
	}

	recorder.Header().Set("Content-Type", "application/json")
	recorder.WriteHeader(http.StatusCreated)
	_, _ = recorder.Write([]byte(`{"id":"123"}`))

	if recorder.statusCode != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, recorder.statusCode)
	}

	if recorder.headers.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", recorder.headers.Get("Content-Type"))
	}

	if recorder.body.String() != `{"id":"123"}` {
		t.Errorf("expected body '%s', got '%s'", `{"id":"123"}`, recorder.body.String())
	}
}

func TestResponseRecorder_DefaultStatus(t *testing.T) {
	recorder := &responseRecorder{
		statusCode: http.StatusOK,
		headers:    make(http.Header),
		body:       &bytes.Buffer{},
	}

	// Write without calling WriteHeader should default to 200
	_, _ = recorder.Write([]byte("test"))

	if recorder.statusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, recorder.statusCode)
	}

	if !recorder.headerWritten {
		t.Error("expected headerWritten to be true")
	}
}

func TestCachedResponseSerialization(t *testing.T) {
	original := &CachedResponse{
		StatusCode: http.StatusCreated,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"Set-Cookie":   {"session=abc", "token=xyz"},
		},
		Body:       `{"id":"12345","status":"created"}`,
		BodyIsText: true,
	}

	// Serialize
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Deserialize
	var restored CachedResponse
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if restored.StatusCode != original.StatusCode {
		t.Errorf("status code mismatch: expected %d, got %d", original.StatusCode, restored.StatusCode)
	}

	if restored.Body != original.Body {
		t.Errorf("body mismatch: expected '%s', got '%s'", original.Body, restored.Body)
	}

	if restored.BodyIsText != original.BodyIsText {
		t.Errorf("bodyIsText mismatch: expected %v, got %v", original.BodyIsText, restored.BodyIsText)
	}

	// Check multi-value headers are preserved
	if len(restored.Headers["Set-Cookie"]) != 2 {
		t.Errorf("expected 2 Set-Cookie values, got %d", len(restored.Headers["Set-Cookie"]))
	}
}

func TestRequestRecordSerialization(t *testing.T) {
	original := &RequestRecord{
		Fingerprint: "abc123def456",
		Status:      statusCompleted,
		Response: &CachedResponse{
			StatusCode: http.StatusOK,
			Headers:    map[string][]string{"Content-Type": {"text/plain"}},
			Body:       "OK",
			BodyIsText: true,
		},
		CreatedAt: 1699999999,
	}

	// Serialize
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Deserialize
	var restored RequestRecord
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if restored.Fingerprint != original.Fingerprint {
		t.Errorf("fingerprint mismatch: expected '%s', got '%s'", original.Fingerprint, restored.Fingerprint)
	}

	if restored.Status != original.Status {
		t.Errorf("status mismatch: expected '%s', got '%s'", original.Status, restored.Status)
	}

	if restored.CreatedAt != original.CreatedAt {
		t.Errorf("createdAt mismatch: expected %d, got %d", original.CreatedAt, restored.CreatedAt)
	}

	if restored.Response == nil {
		t.Fatal("response should not be nil")
	}

	if restored.Response.StatusCode != original.Response.StatusCode {
		t.Errorf("response status mismatch: expected %d, got %d", original.Response.StatusCode, restored.Response.StatusCode)
	}
}

func TestRequestRecord_IsStale(t *testing.T) {
	tests := []struct {
		name        string
		record      *RequestRecord
		lockTimeout int64
		expected    bool
	}{
		{
			name: "completed record is not stale",
			record: &RequestRecord{
				Status:    statusCompleted,
				CreatedAt: time.Now().Unix() - 120, // 2 minutes old
			},
			lockTimeout: 60,
			expected:    false,
		},
		{
			name: "fresh processing record is not stale",
			record: &RequestRecord{
				Status:    statusProcessing,
				CreatedAt: time.Now().Unix() - 30, // 30 seconds old
			},
			lockTimeout: 60,
			expected:    false,
		},
		{
			name: "old processing record is stale",
			record: &RequestRecord{
				Status:    statusProcessing,
				CreatedAt: time.Now().Unix() - 120, // 2 minutes old
			},
			lockTimeout: 60,
			expected:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.record.isStale(tt.lockTimeout)
			if result != tt.expected {
				t.Errorf("expected isStale=%v, got %v", tt.expected, result)
			}
		})
	}
}

func TestIsTextContent(t *testing.T) {
	tests := []struct {
		contentType string
		expected    bool
	}{
		{"", true},
		{"text/plain", true},
		{"text/html", true},
		{"application/json", true},
		{"application/xml", true},
		{"application/javascript", true},
		{"text/xml", true},
		{"application/octet-stream", false},
		{"image/png", false},
		{"application/pdf", false},
	}

	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			result := isTextContent(tt.contentType)
			if result != tt.expected {
				t.Errorf("isTextContent(%q) = %v, expected %v", tt.contentType, result, tt.expected)
			}
		})
	}
}

func TestReadBodyWithLimit(t *testing.T) {
	config := &Config{
		RedisAddress: "localhost:6379",
		MaxBodySize:  100,
	}

	handler := &mockHandler{}
	mw, _ := New(context.Background(), handler, config, "test")
	middleware, ok := mw.(*IdempotencyMiddleware)
	if !ok {
		t.Fatal("failed to cast to IdempotencyMiddleware")
	}

	t.Run("small body", func(t *testing.T) {
		body := bytes.NewReader([]byte("small body"))
		req := httptest.NewRequest(http.MethodPost, "/", body)

		result, err := middleware.readBodyWithLimit(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if string(result) != "small body" {
			t.Errorf("expected 'small body', got '%s'", result)
		}
	})

	t.Run("body exceeds limit", func(t *testing.T) {
		largeBody := bytes.Repeat([]byte("x"), 150)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(largeBody))

		_, err := middleware.readBodyWithLimit(req)
		if err != errBodyTooLarge {
			t.Errorf("expected errBodyTooLarge, got %v", err)
		}
	})

	t.Run("nil body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Body = nil

		result, err := middleware.readBodyWithLimit(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(result) != 0 {
			t.Errorf("expected empty body, got %d bytes", len(result))
		}
	})
}

func TestAcquireResult_Actions(t *testing.T) {
	// Test that action constants are distinct
	actions := []acquireAction{
		actionAcquired,
		actionReplay,
		actionConflict,
		actionFingerprintMismatch,
	}

	seen := make(map[acquireAction]bool)
	for _, action := range actions {
		if seen[action] {
			t.Errorf("duplicate action value: %d", action)
		}
		seen[action] = true
	}
}

func TestRedisClient_ReadLine(t *testing.T) {
	client := newRedisClient("localhost:6379", "", 0, time.Second, 1)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "CRLF line ending",
			input:    "+OK\r\n",
			expected: "+OK",
		},
		{
			name:     "LF only line ending",
			input:    "+OK\n",
			expected: "+OK",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := strings.NewReader(tt.input)
			bufReader := bufio.NewReader(reader)

			result, err := client.readLine(bufReader)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result != tt.expected {
				t.Errorf("expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestFailOpenConfig(t *testing.T) {
	t.Run("failOpen true allows requests through on redis error", func(t *testing.T) {
		failOpen := true
		config := &Config{
			RedisAddress: "invalid-host:6379", // Will fail to connect
			FailOpen:     &failOpen,
		}

		handler := &mockHandler{
			statusCode: http.StatusOK,
			response:   "success",
		}

		middleware, err := New(context.Background(), handler, config, "test")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"test": true}`))
		req.Header.Set("Idempotency-Key", "test-key-123")
		rw := httptest.NewRecorder()

		middleware.ServeHTTP(rw, req)

		// With fail-open, request should pass through to backend
		if handler.called != 1 {
			t.Errorf("expected handler to be called 1 time, got %d", handler.called)
		}
		if rw.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rw.Code)
		}
	})

	t.Run("failOpen false returns 503 on redis error", func(t *testing.T) {
		failOpen := false
		config := &Config{
			RedisAddress: "invalid-host:6379", // Will fail to connect
			FailOpen:     &failOpen,
		}

		handler := &mockHandler{
			statusCode: http.StatusOK,
			response:   "success",
		}

		middleware, err := New(context.Background(), handler, config, "test")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"test": true}`))
		req.Header.Set("Idempotency-Key", "test-key-456")
		rw := httptest.NewRecorder()

		middleware.ServeHTTP(rw, req)

		// With fail-closed, request should be rejected with 503
		if handler.called != 0 {
			t.Errorf("expected handler not to be called, got %d calls", handler.called)
		}
		if rw.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", rw.Code)
		}

		// Verify RFC 7807 problem details
		var problem map[string]interface{}
		if err := json.Unmarshal(rw.Body.Bytes(), &problem); err != nil {
			t.Fatalf("failed to parse problem details: %v", err)
		}

		if problem["type"] != "about:blank#storage-unavailable" {
			t.Errorf("expected type 'about:blank#storage-unavailable', got '%v'", problem["type"])
		}
		status, ok := problem["status"].(float64)
		if !ok || status != 503 {
			t.Errorf("expected status 503, got %v", problem["status"])
		}
	})

	t.Run("failOpen defaults to true when nil", func(t *testing.T) {
		config := &Config{
			RedisAddress: "localhost:6379",
			FailOpen:     nil, // Not set
		}

		config.applyDefaults()

		if config.FailOpen == nil {
			t.Fatal("expected FailOpen to be set after applyDefaults")
		}
		if !*config.FailOpen {
			t.Error("expected FailOpen to default to true")
		}
	})
}
