package httpapi

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// RateLimitConfig tunes the token buckets guarding one route group: one global
// bucket plus one bucket per client IP. Zero fields select that group's defaults.
type RateLimitConfig struct {
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
func DefaultWebhookRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		GlobalPerSecond: 80,
		GlobalBurst:     160,
		PerIPPerSecond:  30,
		PerIPBurst:      60,
	}
}

// DefaultAuthRateLimitConfig returns limits for the public /auth/* routes. A
// person signs in a few times a day, so one IP gets 10 requests a minute after
// a burst of 5 (a login plus its callback and a retry or two). The global
// bucket bounds total work, each callback costing a GitHub round trip and a
// database write, however many IPs the traffic claims to come from.
func DefaultAuthRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		GlobalPerSecond: 5,
		GlobalBurst:     20,
		PerIPPerSecond:  10.0 / 60,
		PerIPBurst:      5,
	}
}

func (c RateLimitConfig) withDefaults(d RateLimitConfig) RateLimitConfig {
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

type ipRateLimiter struct {
	global     *tokenBucket
	perIPRate  float64
	perIPBurst int
	perIPMax   int
	perIPIdle  time.Duration
	perIPMu    sync.Mutex
	perIP      map[string]*perIPLimiterEntry
}

func newIPRateLimiter(cfg, defaults RateLimitConfig) *ipRateLimiter {
	cfg = cfg.withDefaults(defaults)
	return &ipRateLimiter{
		global:     newTokenBucket(cfg.GlobalPerSecond, cfg.GlobalBurst),
		perIPRate:  cfg.PerIPPerSecond,
		perIPBurst: cfg.PerIPBurst,
		perIPMax:   cfg.PerIPMaxEntries,
		perIPIdle:  cfg.PerIPIdle,
		perIP:      make(map[string]*perIPLimiterEntry),
	}
}

func (l *ipRateLimiter) allow(ip string, now time.Time) bool {
	if !l.perIPBucket(ip, now).allow(now) {
		return false
	}
	return l.global.allow(now)
}

func (l *ipRateLimiter) perIPBucket(ip string, now time.Time) *tokenBucket {
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

func (l *ipRateLimiter) evictPerIP(now time.Time) {
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

// trustedProxy reports whether a is loopback or a private IPv4 address (RFC
// 1918, which covers the Docker bridge). IPv6 unique-local and link-local
// addresses are not trusted: a tailnet client has one and must not be skipped.
func trustedProxy(a netip.Addr) bool {
	return a.IsLoopback() || (a.Is4() && a.IsPrivate())
}

// rateLimitKey is the per-client bucket key: an IPv4 address, or an IPv6
// client's /64, since one host can pick any address in its prefix.
func rateLimitKey(a netip.Addr) string {
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}

// clientIP is the rate-limit key: the canonical address of the client. When the
// TCP peer is a trusted proxy (Tailscale Funnel or cloudflared on the host, seen
// as loopback or the Docker bridge gateway), X-Forwarded-For is walked right to
// left past trusted hops and the first other address is the client. Entries a
// client forged sit left of the one the proxy appended, so they are never
// reached. An unparseable entry, or no untrusted entry, falls back to the peer.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return r.RemoteAddr
	}
	peer = peer.Unmap().WithZone("")
	if !trustedProxy(peer) {
		return rateLimitKey(peer)
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return rateLimitKey(peer)
		}
		hop = hop.Unmap().WithZone("")
		if !trustedProxy(hop) {
			return rateLimitKey(hop)
		}
	}
	return rateLimitKey(peer)
}

func withRateLimit(logger *slog.Logger, name string, limiter *ipRateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !limiter.allow(ip, time.Now()) {
			logger.Warn("rate limited", "route_group", name, "client_ip", ip, "delivery_id", r.Header.Get("X-GitHub-Delivery"))
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = r.Body.Close()
			return
		}
		next(w, r)
	}
}
