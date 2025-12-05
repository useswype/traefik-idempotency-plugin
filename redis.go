package traefik_idempotency

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// errNil is returned when Redis returns a nil response (key not found).
var errNil = errors.New("redis: nil")

// redisClient is a minimal Redis client using only the standard library.
// This avoids Yaegi compatibility issues with external Redis libraries.
type redisClient struct {
	address  string
	password string
	db       int
	timeout  time.Duration
	pool     chan *redisConn
}

// redisConn wraps a connection with its buffered reader.
type redisConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

// newRedisClient creates a new Redis client with connection pooling.
func newRedisClient(address, password string, db int, timeout time.Duration, poolSize int) *redisClient {
	return &redisClient{
		address:  address,
		password: password,
		db:       db,
		timeout:  timeout,
		pool:     make(chan *redisConn, poolSize),
	}
}

// getConn retrieves a connection from the pool or creates a new one.
func (c *redisClient) getConn() (*redisConn, error) {
	// Try to get from pool
	select {
	case rc := <-c.pool:
		if err := c.ping(rc); err == nil {
			return rc, nil
		}
		// Connection dead, close and create new
		rc.conn.Close()
	default:
	}

	// Create new connection using Dialer with timeout
	dialer := &net.Dialer{Timeout: c.timeout}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, err
	}

	rc := &redisConn{
		conn:   conn,
		reader: bufio.NewReader(conn),
	}

	// Authenticate if password is set
	if c.password != "" {
		if err := c.auth(rc); err != nil {
			conn.Close()
			return nil, err
		}
	}

	// Select database if not default
	if c.db != 0 {
		if err := c.selectDB(rc); err != nil {
			conn.Close()
			return nil, err
		}
	}

	return rc, nil
}

// releaseConn returns a connection to the pool or closes it.
func (c *redisClient) releaseConn(rc *redisConn, healthy bool) {
	if !healthy {
		rc.conn.Close()
		return
	}

	select {
	case c.pool <- rc:
	default:
		rc.conn.Close()
	}
}

// ping sends a PING command to verify the connection is alive.
func (c *redisClient) ping(rc *redisConn) error {
	if err := rc.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}

	_, err := rc.conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
	if err != nil {
		return err
	}

	line, err := c.readLine(rc.reader)
	if err != nil {
		return err
	}

	if !strings.HasPrefix(line, "+PONG") {
		return errors.New("ping failed")
	}

	return nil
}

// auth authenticates with Redis using the configured password.
func (c *redisClient) auth(rc *redisConn) error {
	cmd := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(c.password), c.password)
	if err := rc.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}

	_, err := rc.conn.Write([]byte(cmd))
	if err != nil {
		return err
	}

	line, err := c.readLine(rc.reader)
	if err != nil {
		return err
	}

	if strings.HasPrefix(line, "-") {
		return errors.New("auth failed: " + line)
	}

	return nil
}

// selectDB selects the configured Redis database.
func (c *redisClient) selectDB(rc *redisConn) error {
	dbStr := strconv.Itoa(c.db)
	cmd := fmt.Sprintf("*2\r\n$6\r\nSELECT\r\n$%d\r\n%s\r\n", len(dbStr), dbStr)
	if err := rc.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}

	_, err := rc.conn.Write([]byte(cmd))
	if err != nil {
		return err
	}

	line, err := c.readLine(rc.reader)
	if err != nil {
		return err
	}

	if strings.HasPrefix(line, "-") {
		return errors.New("select failed: " + line)
	}

	return nil
}

// get retrieves a value from Redis by key.
func (c *redisClient) get(key string) (string, error) {
	rc, err := c.getConn()
	if err != nil {
		return "", err
	}

	healthy := true
	defer func() { c.releaseConn(rc, healthy) }()

	cmd := fmt.Sprintf("*2\r\n$3\r\nGET\r\n$%d\r\n%s\r\n", len(key), key)
	if err := rc.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		healthy = false
		return "", err
	}

	_, err = rc.conn.Write([]byte(cmd))
	if err != nil {
		healthy = false
		return "", err
	}

	result, err := c.readBulkString(rc.reader)
	if err != nil {
		if !errors.Is(err, errNil) {
			healthy = false
		}
		return "", err
	}

	return result, nil
}

// setNX sets a key only if it doesn't exist, with expiration.
// Returns true if the key was set, false if it already existed.
func (c *redisClient) setNX(key, value string, ttlSeconds int) (bool, error) {
	rc, err := c.getConn()
	if err != nil {
		return false, err
	}

	healthy := true
	defer func() { c.releaseConn(rc, healthy) }()

	// SET key value NX EX seconds
	ttlStr := strconv.Itoa(ttlSeconds)
	cmd := fmt.Sprintf("*6\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n$2\r\nNX\r\n$2\r\nEX\r\n$%d\r\n%s\r\n",
		len(key), key, len(value), value, len(ttlStr), ttlStr)

	if err := rc.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		healthy = false
		return false, err
	}

	_, err = rc.conn.Write([]byte(cmd))
	if err != nil {
		healthy = false
		return false, err
	}

	line, err := c.readLine(rc.reader)
	if err != nil {
		healthy = false
		return false, err
	}

	// +OK means set succeeded, $-1 (nil) means key existed
	return strings.HasPrefix(line, "+OK"), nil
}

// setEX sets a key with expiration, overwriting any existing value.
func (c *redisClient) setEX(key, value string, ttlSeconds int) error {
	rc, err := c.getConn()
	if err != nil {
		return err
	}

	healthy := true
	defer func() { c.releaseConn(rc, healthy) }()

	// SET key value EX seconds
	ttlStr := strconv.Itoa(ttlSeconds)
	cmd := fmt.Sprintf("*5\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n$2\r\nEX\r\n$%d\r\n%s\r\n",
		len(key), key, len(value), value, len(ttlStr), ttlStr)

	if err := rc.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		healthy = false
		return err
	}

	_, err = rc.conn.Write([]byte(cmd))
	if err != nil {
		healthy = false
		return err
	}

	line, err := c.readLine(rc.reader)
	if err != nil {
		healthy = false
		return err
	}

	if strings.HasPrefix(line, "-") {
		return errors.New("set failed: " + line)
	}

	return nil
}

// readLine reads a line and strips CRLF or LF.
func (c *redisClient) readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	// Handle both \r\n and \n line endings
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")

	return line, nil
}

// readBulkString reads a RESP bulk string response.
func (c *redisClient) readBulkString(reader *bufio.Reader) (string, error) {
	line, err := c.readLine(reader)
	if err != nil {
		return "", err
	}

	if len(line) == 0 {
		return "", errors.New("empty response")
	}

	switch line[0] {
	case '$':
		// Bulk string
		length, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", err
		}
		if length == -1 {
			return "", errNil
		}
		data := make([]byte, length+2) // +2 for \r\n
		_, err = io.ReadFull(reader, data)
		if err != nil {
			return "", err
		}
		return string(data[:length]), nil

	case '+':
		// Simple string
		return line[1:], nil

	case '-':
		// Error
		return "", errors.New(line[1:])

	default:
		return "", fmt.Errorf("unexpected response type: %c", line[0])
	}
}
