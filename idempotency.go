// Package traefik_idempotency implements RFC draft-ietf-httpapi-idempotency-key-header
// as a Traefik middleware plugin using Redis for storage.
//
// This package provides idempotent request handling for HTTP APIs, ensuring that
// duplicate requests with the same Idempotency-Key header return cached responses
// instead of being processed multiple times.
//
// Files:
//   - config.go: Configuration types and defaults
//   - redis.go: Minimal Redis client implementation
//   - response.go: Response types and recorder
//   - middleware.go: Main middleware handler logic
package traefik_idempotency
