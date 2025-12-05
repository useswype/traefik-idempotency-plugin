package traefik_idempotency

import (
	"fmt"
	"net"
	"strings"
)

// Default configuration values.
const (
	defaultRedisAddress      = "localhost:6379"
	defaultKeyHeader         = "Idempotency-Key"
	defaultTTLSeconds        = 86400
	defaultKeyPrefix         = "idempotency:"
	defaultConnectionTimeout = 5000
	defaultPoolSize          = 10
	defaultMaxBodySize       = 1 << 20 // 1MB
	defaultLockTimeout       = 60
)

// Config holds the plugin configuration.
type Config struct {
	// RedisAddress is the address of the Redis server (host:port).
	RedisAddress string `json:"redisAddress,omitempty"`

	// RedisPassword is the optional password for Redis authentication.
	RedisPassword string `json:"redisPassword,omitempty"`

	// RedisDB is the Redis database number to use.
	RedisDB int `json:"redisDB,omitempty"`

	// KeyHeader is the name of the idempotency key header.
	// Defaults to "Idempotency-Key" per RFC.
	KeyHeader string `json:"keyHeader,omitempty"`

	// TTLSeconds is how long to store idempotency records in Redis.
	// Defaults to 86400 (24 hours).
	TTLSeconds int `json:"ttlSeconds,omitempty"`

	// RequireKey when true returns 400 if the idempotency key is missing.
	// When false, requests without keys pass through normally.
	RequireKey bool `json:"requireKey,omitempty"`

	// Methods is the list of HTTP methods to enforce idempotency on.
	// Defaults to ["POST", "PATCH"].
	Methods []string `json:"methods,omitempty"`

	// KeyPrefix is a prefix for all Redis keys to avoid collisions.
	KeyPrefix string `json:"keyPrefix,omitempty"`

	// ConnectionTimeout is the timeout for Redis connections in milliseconds.
	ConnectionTimeout int `json:"connectionTimeout,omitempty"`

	// PoolSize is the maximum number of Redis connections to pool.
	PoolSize int `json:"poolSize,omitempty"`

	// MaxBodySize is the maximum request body size to buffer in bytes.
	// Defaults to 1MB. Requests larger than this will pass through without idempotency.
	MaxBodySize int64 `json:"maxBodySize,omitempty"`

	// LockTimeout is how long a "processing" lock is considered valid in seconds.
	// After this time, the lock is considered stale and can be overwritten.
	// Defaults to 60 seconds.
	LockTimeout int `json:"lockTimeout,omitempty"`

	// FailOpen when true allows requests through if Redis is unavailable.
	// When false, returns 503 Service Unavailable on Redis errors.
	// Defaults to true.
	FailOpen *bool `json:"failOpen,omitempty"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	failOpen := true
	return &Config{
		RedisAddress:      defaultRedisAddress,
		KeyHeader:         defaultKeyHeader,
		TTLSeconds:        defaultTTLSeconds,
		RequireKey:        false,
		Methods:           []string{"POST", "PATCH"},
		KeyPrefix:         defaultKeyPrefix,
		ConnectionTimeout: defaultConnectionTimeout,
		PoolSize:          defaultPoolSize,
		MaxBodySize:       defaultMaxBodySize,
		LockTimeout:       defaultLockTimeout,
		FailOpen:          &failOpen,
	}
}

// Validate checks the configuration for errors.
// This should be called during plugin initialization to fail fast.
func (c *Config) Validate() error {
	if c.RedisAddress == "" {
		return fmt.Errorf("redisAddress is required")
	}

	// Validate Redis address format
	host, port, err := net.SplitHostPort(c.RedisAddress)
	if err != nil {
		return fmt.Errorf("invalid redisAddress format (expected host:port): %w", err)
	}
	if host == "" {
		return fmt.Errorf("redisAddress host cannot be empty")
	}
	if port == "" {
		return fmt.Errorf("redisAddress port cannot be empty")
	}

	// Validate Redis DB range
	if c.RedisDB < 0 || c.RedisDB > 15 {
		return fmt.Errorf("redisDB must be between 0 and 15, got %d", c.RedisDB)
	}

	// Validate methods
	validMethods := map[string]bool{
		"GET": true, "POST": true, "PUT": true, "PATCH": true,
		"DELETE": true, "HEAD": true, "OPTIONS": true,
	}
	for _, method := range c.Methods {
		if !validMethods[strings.ToUpper(method)] {
			return fmt.Errorf("invalid HTTP method: %s", method)
		}
	}

	// Validate numeric ranges
	if c.TTLSeconds < 0 {
		return fmt.Errorf("ttlSeconds cannot be negative")
	}
	if c.ConnectionTimeout < 0 {
		return fmt.Errorf("connectionTimeout cannot be negative")
	}
	if c.PoolSize < 0 {
		return fmt.Errorf("poolSize cannot be negative")
	}
	if c.MaxBodySize < 0 {
		return fmt.Errorf("maxBodySize cannot be negative")
	}
	if c.LockTimeout < 0 {
		return fmt.Errorf("lockTimeout cannot be negative")
	}

	return nil
}

// applyDefaults fills in default values for any unset configuration fields.
func (c *Config) applyDefaults() {
	if c.KeyHeader == "" {
		c.KeyHeader = defaultKeyHeader
	}
	if c.TTLSeconds <= 0 {
		c.TTLSeconds = defaultTTLSeconds
	}
	if len(c.Methods) == 0 {
		c.Methods = []string{"POST", "PATCH"}
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = defaultKeyPrefix
	}
	if c.ConnectionTimeout <= 0 {
		c.ConnectionTimeout = defaultConnectionTimeout
	}
	if c.PoolSize <= 0 {
		c.PoolSize = defaultPoolSize
	}
	if c.MaxBodySize <= 0 {
		c.MaxBodySize = defaultMaxBodySize
	}
	if c.LockTimeout <= 0 {
		c.LockTimeout = defaultLockTimeout
	}
	if c.FailOpen == nil {
		failOpen := true
		c.FailOpen = &failOpen
	}
}
