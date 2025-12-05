# Traefik Idempotency Middleware

A Traefik middleware plugin that implements [RFC draft-ietf-httpapi-idempotency-key-header](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/) for idempotent HTTP requests using Redis for storage.

## Features

- **RFC Compliant**: Implements the IETF Idempotency-Key HTTP Header draft specification
- **Redis Storage**: Uses Redis for distributed storage of idempotency records
- **Concurrent Request Handling**: Properly handles concurrent duplicate requests with 409 Conflict
- **Request Fingerprinting**: Detects when the same key is reused with different payloads (422 Unprocessable Content)
- **Response Caching**: Caches and replays responses for duplicate requests
- **RFC 7807 Errors**: Returns problem details format for error responses
- **Binary-Safe**: Properly handles binary response bodies using base64 encoding
- **Multi-Value Headers**: Preserves all header values including Set-Cookie
- **Stale Lock Recovery**: Automatically recovers from crashed requests via configurable lock timeout

## How It Works

```
┌─────────────┐     ┌──────────────────────┐     ┌─────────┐
│   Client    │────▶│  Idempotency Plugin  │────▶│ Backend │
└─────────────┘     └──────────────────────┘     └─────────┘
                              │
                              ▼
                        ┌─────────┐
                        │  Redis  │
                        └─────────┘
```

1. **First request**: Acquires lock, processes request, caches response
2. **Duplicate (completed)**: Returns cached response immediately
3. **Concurrent retry**: Returns 409 Conflict if original still processing
4. **Stale lock**: If processing lock exceeds `lockTimeout`, overwrites and reprocesses

## Installation

### Plugin Catalog (Recommended)

Add to your Traefik static configuration:

```yaml
experimental:
  plugins:
    idempotency:
      moduleName: github.com/useswype/traefik-idempotency-plugin
      version: v1.0.0
```

### Local Development

Place the plugin in your Traefik plugins directory:

```
./plugins-local/
└── src
    └── github.com
        └── useswype
            └── traefik-idempotency-plugin
                ├── *.go
                ├── go.mod
                └── .traefik.yml
```

```yaml
experimental:
  localPlugins:
    idempotency:
      moduleName: github.com/useswype/traefik-idempotency-plugin
```

## Configuration

### Configuration Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `redisAddress` | string | `localhost:6379` | Redis server address (host:port) |
| `redisPassword` | string | `""` | Redis authentication password |
| `redisDB` | int | `0` | Redis database number (0-15) |
| `keyPrefix` | string | `idempotency:` | Redis key prefix |
| `keyHeader` | string | `Idempotency-Key` | Header name for the idempotency key |
| `ttlSeconds` | int | `86400` | TTL for stored records (24 hours) |
| `requireKey` | bool | `false` | Return 400 if key is missing |
| `methods` | []string | `["POST", "PATCH"]` | HTTP methods to enforce idempotency |
| `connectionTimeout` | int | `5000` | Redis connection timeout (ms) |
| `poolSize` | int | `10` | Max Redis connections to pool |
| `maxBodySize` | int | `1048576` | Max body size to buffer (1MB) |
| `lockTimeout` | int | `60` | Seconds before processing lock is stale |
| `failOpen` | bool | `true` | Allow requests through if Redis is unavailable |

### Kubernetes Example

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: idempotency
  namespace: default
spec:
  plugin:
    idempotency:
      redisAddress: "redis-master.redis.svc.cluster.local:6379"
      redisPassword: ""
      redisDB: 0
      keyHeader: "Idempotency-Key"
      ttlSeconds: 86400
      requireKey: false
      methods:
        - POST
        - PATCH
        - PUT
      keyPrefix: "idempotency:"
      connectionTimeout: 5000
      poolSize: 10
      maxBodySize: 1048576
      lockTimeout: 60
      failOpen: true
---
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: api
  namespace: default
spec:
  entryPoints:
    - web
  routes:
    - match: Host(`api.example.com`)
      kind: Rule
      middlewares:
        - name: idempotency
      services:
        - name: api-service
          port: 80
```

## Examples

### Client Request
```bash
# First request - creates the resource
curl -X POST https://api.example.com/orders \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: "8e03978e-40d5-43e8-bc93-6894a57f9324"' \
  -d '{"amount": 100, "currency": "USD"}'

# Response: 201 Created
# {"id": "order-123", "amount": 100, "currency": "USD"}

# Retry with same key - returns cached response instantly
curl -X POST https://api.example.com/orders \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: "8e03978e-40d5-43e8-bc93-6894a57f9324"' \
  -d '{"amount": 100, "currency": "USD"}'

# Response: 201 Created (from cache)
# {"id": "order-123", "amount": 100, "currency": "USD"}
```

### Idempotency Key Format

Per RFC 8941, the idempotency key should be a quoted string:

```
Idempotency-Key: "8e03978e-40d5-43e8-bc93-6894a57f9324"
```

The plugin also accepts unquoted values for convenience:

```
Idempotency-Key: 8e03978e-40d5-43e8-bc93-6894a57f9324
```

Valid key characters: `a-z`, `A-Z`, `0-9`, `-`, `_`

### Error Responses

All errors follow RFC 7807 Problem Details format:

**400 Bad Request - Missing Key (when required)**
```json
{
  "type": "about:blank#missing-idempotency-key",
  "title": "Bad Request",
  "status": 400,
  "detail": "The Idempotency-Key header is required for this request"
}
```

**409 Conflict - Concurrent Request**
```json
{
  "type": "about:blank#request-in-progress",
  "title": "Conflict",
  "status": 409,
  "detail": "A request with this Idempotency-Key is currently being processed"
}
```

**422 Unprocessable Content - Key Reused**
```json
{
  "type": "about:blank#idempotency-key-reused",
  "title": "Unprocessable Content",
  "status": 422,
  "detail": "The Idempotency-Key has already been used with a different request payload"
}
```

**503 Service Unavailable - Storage Unavailable (when failOpen=false)**
```json
{
  "type": "about:blank#storage-unavailable",
  "title": "Service Unavailable",
  "status": 503,
  "detail": "The idempotency storage is temporarily unavailable"
}
```

## Architecture

### Redis Client

This plugin uses a custom minimal Redis client built on Go's standard library. This design choice was made because:

1. Traefik plugins run via Yaegi (Go interpreter), which has compatibility issues with popular Redis client libraries
2. The custom client implements only the necessary RESP protocol commands
3. Connection pooling is included to minimize connection overhead