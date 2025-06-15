// Package ratelimiter provides a Token Bucket rate limiting plugin for GoFr
package ratelimiter

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gofr.dev/pkg/gofr"
)

const (
	// Plugin constants
	PluginName    = "token-bucket-rate-limiter"
	PluginVersion = "1.0.0"
)

// Config holds the configuration for the rate limiter plugin
type Config struct {
	Capacity         int           `json:"capacity" yaml:"capacity"`                   // Token bucket capacity
	RefillRate       int           `json:"refill_rate" yaml:"refill_rate"`             // Tokens per second
	UserIDHeader     string        `json:"user_id_header" yaml:"user_id_header"`       // Header to extract user ID
	UseIPFallback    bool          `json:"use_ip_fallback" yaml:"use_ip_fallback"`     // Use IP if no user ID header
	SkipPaths        []string      `json:"skip_paths" yaml:"skip_paths"`               // Paths to skip rate limiting
	CustomKeyFunc    KeyExtractor  `json:"-" yaml:"-"`                                 // Custom key extraction function
	CleanupInterval  time.Duration `json:"cleanup_interval" yaml:"cleanup_interval"`   // How often to cleanup old buckets
	BucketTTL        time.Duration `json:"bucket_ttl" yaml:"bucket_ttl"`               // How long to keep inactive buckets
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		Capacity:        100,
		RefillRate:      2,
		UserIDHeader:    "X-User-ID",
		UseIPFallback:   true,
		SkipPaths:       []string{"/health", "/metrics"},
		CleanupInterval: 5 * time.Minute,
		BucketTTL:       10 * time.Minute,
	}
}

// KeyExtractor is a function type for extracting rate limit keys from requests
type KeyExtractor func(*http.Request) string

// AtomicTokenBucket represents a thread-safe token bucket using atomic operations
type AtomicTokenBucket struct {
	capacity     int64
	tokens       int64
	refillRate   int64
	lastRefill   int64 // Unix nano timestamp
	lastAccess   int64 // For cleanup purposes
	refillPeriod int64 // Nanoseconds between token additions
}

// NewAtomicTokenBucket creates a new atomic token bucket
func NewAtomicTokenBucket(capacity, refillRate int) *AtomicTokenBucket {
	now := time.Now().UnixNano()
	refillPeriod := int64(time.Second) / int64(refillRate)
	
	return &AtomicTokenBucket{
		capacity:     int64(capacity),
		tokens:       int64(capacity),
		refillRate:   int64(refillRate),
		lastRefill:   now,
		lastAccess:   now,
		refillPeriod: refillPeriod,
	}
}

// TryConsume attempts to consume a token
func (tb *AtomicTokenBucket) TryConsume() bool {
	now := time.Now().UnixNano()
	atomic.StoreInt64(&tb.lastAccess, now)
	
	// Refill tokens
	tb.refill(now)
	
	// Try to consume a token atomically
	for {
		current := atomic.LoadInt64(&tb.tokens)
		if current <= 0 {
			return false
		}
		
		if atomic.CompareAndSwapInt64(&tb.tokens, current, current-1) {
			return true
		}
	}
}

// refill adds tokens based on elapsed time
func (tb *AtomicTokenBucket) refill(now int64) {
	lastRefill := atomic.LoadInt64(&tb.lastRefill)
	elapsed := now - lastRefill
	
	if elapsed <= 0 {
		return
	}
	
	tokensToAdd := elapsed / tb.refillPeriod
	if tokensToAdd <= 0 {
		return
	}
	
	// Update lastRefill atomically
	if !atomic.CompareAndSwapInt64(&tb.lastRefill, lastRefill, now) {
		return
	}
	
	// Add tokens atomically
	for {
		current := atomic.LoadInt64(&tb.tokens)
		newTokens := current + tokensToAdd
		if newTokens > tb.capacity {
			newTokens = tb.capacity
		}
		
		if atomic.CompareAndSwapInt64(&tb.tokens, current, newTokens) {
			break
		}
	}
}

// GetStatus returns current bucket status
func (tb *AtomicTokenBucket) GetStatus() (tokens, capacity int64) {
	tb.refill(time.Now().UnixNano())
	return atomic.LoadInt64(&tb.tokens), tb.capacity
}

// IsExpired checks if bucket hasn't been accessed recently
func (tb *AtomicTokenBucket) IsExpired(ttl time.Duration) bool {
	lastAccess := atomic.LoadInt64(&tb.lastAccess)
	return time.Since(time.Unix(0, lastAccess)) > ttl
}

// Plugin represents the rate limiter plugin
type Plugin struct {
	config  *Config
	buckets sync.Map // map[string]*AtomicTokenBucket
	ticker  *time.Ticker
	done    chan struct{}
}

// NewPlugin creates a new rate limiter plugin instance
func NewPlugin(config *Config) *Plugin {
	if config == nil {
		config = DefaultConfig()
	}
	
	plugin := &Plugin{
		config: config,
		done:   make(chan struct{}),
	}
	
	// Start cleanup routine
	if config.CleanupInterval > 0 && config.BucketTTL > 0 {
		plugin.startCleanup()
	}
	
	return plugin
}

// Name returns the plugin name
func (p *Plugin) Name() string {
	return PluginName
}

// Version returns the plugin version
func (p *Plugin) Version() string {
	return PluginVersion
}

// Initialize initializes the plugin (implements gofr.Plugin interface)
func (p *Plugin) Initialize(ctx context.Context) error {
	return nil
}

// Shutdown cleans up plugin resources
func (p *Plugin) Shutdown(ctx context.Context) error {
	close(p.done)
	if p.ticker != nil {
		p.ticker.Stop()
	}
	return nil
}

// Middleware returns the rate limiting middleware
func (p *Plugin) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip rate limiting for specified paths
			for _, path := range p.config.SkipPaths {
				if r.URL.Path == path {
					next.ServeHTTP(w, r)
					return
				}
			}
			
			// Extract key for rate limiting
			key := p.extractKey(r)
			if key == "" {
				// If no key can be extracted, allow the request
				next.ServeHTTP(w, r)
				return
			}
			
			// Check rate limit
			if !p.IsAllowed(key) {
				p.handleRateLimitExceeded(w, key)
				return
			}
			
			// Add rate limit headers
			p.addRateLimitHeaders(w, key)
			
			next.ServeHTTP(w, r)
		})
	}
}

// IsAllowed checks if a request with the given key is allowed
func (p *Plugin) IsAllowed(key string) bool {
	bucket := p.getBucket(key)
	return bucket.TryConsume()
}

// GetStatus returns the current status for a key
func (p *Plugin) GetStatus(key string) (remaining, limit int64, exists bool) {
	if bucketInterface, ok := p.buckets.Load(key); ok {
		bucket := bucketInterface.(*AtomicTokenBucket)
		tokens, capacity := bucket.GetStatus()
		return tokens, capacity, true
	}
	return int64(p.config.Capacity), int64(p.config.Capacity), false
}

// getBucket gets or creates a bucket for the given key
func (p *Plugin) getBucket(key string) *AtomicTokenBucket {
	if bucketInterface, exists := p.buckets.Load(key); exists {
		return bucketInterface.(*AtomicTokenBucket)
	}
	
	// Create new bucket
	bucket := NewAtomicTokenBucket(p.config.Capacity, p.config.RefillRate)
	actual, _ := p.buckets.LoadOrStore(key, bucket)
	return actual.(*AtomicTokenBucket)
}

// extractKey extracts the rate limiting key from the request
func (p *Plugin) extractKey(r *http.Request) string {
	// Use custom key extractor if provided
	if p.config.CustomKeyFunc != nil {
		return p.config.CustomKeyFunc(r)
	}
	
	// Try to get user ID from header
	if p.config.UserIDHeader != "" {
		if userID := r.Header.Get(p.config.UserIDHeader); userID != "" {
			return userID
		}
	}
	
	// Fallback to IP address if enabled
	if p.config.UseIPFallback {
		return r.RemoteAddr
	}
	
	return ""
}

// handleRateLimitExceeded handles rate limit exceeded responses
func (p *Plugin) handleRateLimitExceeded(w http.ResponseWriter, key string) {
	remaining, limit, _ := p.GetStatus(key)
	
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(limit, 10))
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
	w.Header().Set("Retry-After", "1") // Suggest retry after 1 second
	w.WriteHeader(http.StatusTooManyRequests)
	
	response := fmt.Sprintf(`{
		"error": "Rate limit exceeded",
		"message": "Too many requests. Please try again later.",
		"remaining": %d,
		"limit": %d,
		"retry_after": 1
	}`, remaining, limit)
	
	w.Write([]byte(response))
}

// addRateLimitHeaders adds rate limit headers to the response
func (p *Plugin) addRateLimitHeaders(w http.ResponseWriter, key string) {
	remaining, limit, _ := p.GetStatus(key)
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(limit, 10))
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
}

// startCleanup starts the background cleanup routine
func (p *Plugin) startCleanup() {
	p.ticker = time.NewTicker(p.config.CleanupInterval)
	
	go func() {
		for {
			select {
			case <-p.ticker.C:
				p.cleanup()
			case <-p.done:
				return
			}
		}
	}()
}

// cleanup removes expired buckets
func (p *Plugin) cleanup() {
	p.buckets.Range(func(key, value interface{}) bool {
		bucket := value.(*AtomicTokenBucket)
		if bucket.IsExpired(p.config.BucketTTL) {
			p.buckets.Delete(key)
		}
		return true
	})
}

// StatusHandler returns a handler for checking rate limit status
func (p *Plugin) StatusHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := p.extractKey(r)
		if key == "" {
			http.Error(w, "Cannot extract rate limit key", http.StatusBadRequest)
			return
		}
		
		remaining, limit, exists := p.GetStatus(key)
		
		response := map[string]interface{}{
			"key":       key,
			"remaining": remaining,
			"limit":     limit,
			"exists":    exists,
			"config": map[string]interface{}{
				"capacity":    p.config.Capacity,
				"refill_rate": p.config.RefillRate,
			},
		}
		
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"key": "%s",
			"remaining": %d,
			"limit": %d,
			"exists": %t,
			"config": {
				"capacity": %d,
				"refill_rate": %d
			}
		}`, response["key"], response["remaining"], response["limit"], 
		response["exists"], p.config.Capacity, p.config.RefillRate)
	}
}

// --- Example usage ---

func ExampleUsage() {
	// Create GoFr app
	app := gofr.New()
	
	// Create plugin with custom config
	config := &Config{
		Capacity:     50,  // 50 tokens per bucket
		RefillRate:   5,   // 5 tokens per second
		UserIDHeader: "X-User-ID",
		UseIPFallback: true,
		SkipPaths:    []string{"/health", "/metrics", "/status"},
		CustomKeyFunc: func(r *http.Request) string {
			// Custom key extraction logic
			if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
				return "api:" + apiKey
			}
			if userID := r.Header.Get("X-User-ID"); userID != "" {
				return "user:" + userID
			}
			return "ip:" + r.RemoteAddr
		},
		CleanupInterval: 2 * time.Minute,
		BucketTTL:      5 * time.Minute,
	}
	
	// Initialize plugin
	rateLimiter := NewPlugin(config)
	
	// Register middleware
	app.UseMiddleware(rateLimiter.Middleware())
	
	// Register status endpoint
	app.GET("/ratelimit/status", func(ctx *gofr.Context) (interface{}, error) {
		handler := rateLimiter.StatusHandler()
		// Convert GoFr context to standard HTTP
		handler.ServeHTTP(ctx.ResponseWriter, ctx.Request)
		return nil, nil
	})
	
	// Your API endpoints
	app.GET("/api/data", func(ctx *gofr.Context) (interface{}, error) {
		return map[string]string{"message": "Success!"}, nil
	})
	
	// Health check (not rate limited)
	app.GET("/health", func(ctx *gofr.Context) (interface{}, error) {
		return map[string]string{"status": "healthy"}, nil
	})
	
	// Shutdown cleanup
	defer rateLimiter.Shutdown(context.Background())
	
	app.Start()
}

// Helper functions for easier integration

// WithCapacity sets the bucket capacity
func WithCapacity(capacity int) func(*Config) {
	return func(c *Config) {
		c.Capacity = capacity
	}
}

// WithRefillRate sets the refill rate
func WithRefillRate(rate int) func(*Config) {
	return func(c *Config) {
		c.RefillRate = rate
	}
}

// WithCustomKeyExtractor sets a custom key extraction function
func WithCustomKeyExtractor(extractor KeyExtractor) func(*Config) {
	return func(c *Config) {
		c.CustomKeyFunc = extractor
	}
}

// WithSkipPaths sets paths to skip rate limiting
func WithSkipPaths(paths []string) func(*Config) {
	return func(c *Config) {
		c.SkipPaths = paths
	}
}

// NewPluginWithOptions creates a plugin with configuration options
func NewPluginWithOptions(options ...func(*Config)) *Plugin {
	config := DefaultConfig()
	for _, option := range options {
		option(config)
	}
	return NewPlugin(config)
}
