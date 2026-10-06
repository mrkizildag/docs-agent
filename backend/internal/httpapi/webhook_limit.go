package httpapi

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WebhookRateLimitConfig tunes token buckets for POST /webhook. Zero values select
// defaults sized for GitHub delivery bursts at a single installation.
type WebhookRateLimitConfig struct {
	GlobalPerSecond float64
	GlobalBurst     int
	PerIPPerSecond  float64
	PerIPBurst      int
	// PerIPMaxEntries caps distinct client IPs tracked at once; zero = default.
	PerIPMaxEntries int
	// PerIPIdle drops a per-IP bucket after this long without a request; zero = default.
	PerIPIdle time.Duration
}

// DefaultWebhookRateLimitConfig returns limits that should not throttle normal
// GitHub webhook volume for one App installation.
func DefaultWebhookRateLimitConfig() WebhookRateLimitConfig {
	return WebhookRateLimitConfig{
		GlobalPerSecond: 80,
		GlobalBurst:     160,
		PerIPPerSecond:  30,
		PerIPBurst:      60,
	}
}

func (c WebhookRateLimitConfig) withDefaults() WebhookRateLimitConfig {
	d := DefaultWebhookRateLimitConfig()
	if c.GlobalPerSecond <= 0 {
		c.GlobalPerSecond = d.GlobalPerSecond
	}
	if c.GlobalBurst <= 0 {
		c.GlobalBurst = d.GlobalBurst
	}
	if c.PerIPPerSecond <= 0 {
		c.PerIPPerSecond = d.PerIPPerSecond
	}
	if c.PerIPBurst <= 0 {
		c.PerIPBurst = d.PerIPBurst
	}
	if c.PerIPMaxEntries <= 0 {
		c.PerIPMaxEntries = 2048
	}
	if c.PerIPIdle <= 0 {
		c.PerIPIdle = 15 * time.Minute
	}
	return c
}

type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(perSecond float64, burst int) *tokenBucket {
	b := float64(burst)
	return &tokenBucket{
		rate:   perSecond,
		burst:  b,
		tokens: b,
		last:   time.Now(),
	}
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type perIPLimiterEntry struct {
	lim      *tokenBucket
	lastSeen time.Time
}

type webhookRateLimiter struct {
	global     *tokenBucket
	perIPRate  float64
	perIPBurst int
	perIPMax   int
	perIPIdle  time.Duration
	perIPMu    sync.Mutex
	perIP      map[string]*perIPLimiterEntry
}

func newWebhookRateLimiter(cfg WebhookRateLimitConfig) *webhookRateLimiter {
	cfg = cfg.withDefaults()
	return &webhookRateLimiter{
		global:     newTokenBucket(cfg.GlobalPerSecond, cfg.GlobalBurst),
		perIPRate:  cfg.PerIPPerSecond,
		perIPBurst: cfg.PerIPBurst,
		perIPMax:   cfg.PerIPMaxEntries,
		perIPIdle:  cfg.PerIPIdle,
		perIP:      make(map[string]*perIPLimiterEntry),
	}
}

func (l *webhookRateLimiter) allow(ip string, now time.Time) bool {
	if !l.perIPBucket(ip, now).allow(now) {
		return false
	}
	return l.global.allow(now)
}

func (l *webhookRateLimiter) perIPBucket(ip string, now time.Time) *tokenBucket {
	l.perIPMu.Lock()
	defer l.perIPMu.Unlock()
	if e, ok := l.perIP[ip]; ok {
		e.lastSeen = now
		return e.lim
	}
	l.evictPerIP(now)
	lim := newTokenBucket(l.perIPRate, l.perIPBurst)
	l.perIP[ip] = &perIPLimiterEntry{lim: lim, lastSeen: now}
	return lim
}

func (l *webhookRateLimiter) evictPerIP(now time.Time) {
	cutoff := now.Add(-l.perIPIdle)
	for ip, e := range l.perIP {
		if e.lastSeen.Before(cutoff) {
			delete(l.perIP, ip)
		}
	}
	for len(l.perIP) >= l.perIPMax {
		var oldestIP string
		var oldest time.Time
		first := true
		for ip, e := range l.perIP {
			if first || e.lastSeen.Before(oldest) {
				oldestIP = ip
				oldest = e.lastSeen
				first = false
			}
		}
		if first {
			return
		}
		delete(l.perIP, oldestIP)
	}
}

func peerIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.HasPrefix(r.RemoteAddr, "127.") || strings.HasPrefix(r.RemoteAddr, "[::1]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// webhookClientIP is the rate-limit key. Behind Tailscale Funnel the TCP peer is
// localhost; Funnel sets X-Forwarded-For to the real client. The header is used
// only when the peer is loopback so direct clients cannot forge it.
func webhookClientIP(r *http.Request) string {
	if peerIsLoopback(r) {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			part, _, _ := strings.Cut(fwd, ",")
			if ip := strings.TrimSpace(part); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func withWebhookRateLimit(logger *slog.Logger, limiter *webhookRateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := webhookClientIP(r)
		if !limiter.allow(ip, time.Now()) {
			logger.Warn("webhook rate limited", "client_ip", ip, "delivery_id", r.Header.Get("X-GitHub-Delivery"))
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = r.Body.Close()
			return
		}
		next(w, r)
	}
}
