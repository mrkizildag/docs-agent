package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
)

func TestClientIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		peer string
		xff  []string
		want string
	}{
		{name: "docker bridge peer with one hop", peer: "172.18.0.1:5555", xff: []string{"1.2.3.4"}, want: "1.2.3.4"},
		{name: "loopback peer", peer: "127.0.0.1:5555", xff: []string{"1.2.3.4"}, want: "1.2.3.4"},
		{name: "forged leftmost entry", peer: "172.18.0.1:5555", xff: []string{"6.6.6.6, 1.2.3.4"}, want: "1.2.3.4"},
		{name: "forged entry in a separate header", peer: "172.18.0.1:5555", xff: []string{"6.6.6.6", "1.2.3.4"}, want: "1.2.3.4"},
		{name: "trusted hops are skipped", peer: "172.18.0.1:5555", xff: []string{"1.2.3.4, 10.0.0.7, 127.0.0.1"}, want: "1.2.3.4"},
		{name: "ipv6 client is canonical", peer: "[::1]:5555", xff: []string{"2001:DB8:0:0::1"}, want: "2001:db8::/64"},
		{name: "unique local peer is not trusted", peer: "[fd00::2]:5555", xff: []string{"1.2.3.4"}, want: "fd00::/64"},
		{name: "link-local peer is not trusted", peer: "[fe80::2]:5555", xff: []string{"1.2.3.4"}, want: "fe80::/64"},
		{name: "tailnet ipv6 hop is the client", peer: "172.18.0.1:5555", xff: []string{"1.2.3.4, fd7a:115c:a1e0::1"}, want: "fd7a:115c:a1e0::/64"},
		{name: "ipv6 clients in one /64 share a key", peer: "[::1]:5555", xff: []string{"2001:db8:0:1:aaaa::1"}, want: "2001:db8:0:1::/64"},
		{name: "another address in that /64", peer: "[::1]:5555", xff: []string{"2001:db8:0:1:bbbb::2"}, want: "2001:db8:0:1::/64"},
		{name: "public peer ignores the header", peer: "203.0.113.10:1234", xff: []string{"1.2.3.4"}, want: "203.0.113.10"},
		{name: "garbage entry", peer: "172.18.0.1:5555", xff: []string{"not-an-ip"}, want: "172.18.0.1"},
		{name: "garbage left of a good entry", peer: "172.18.0.1:5555", xff: []string{"garbage, 1.2.3.4"}, want: "1.2.3.4"},
		{name: "garbage right of a good entry", peer: "172.18.0.1:5555", xff: []string{"1.2.3.4, garbage"}, want: "172.18.0.1"},
		{name: "no header", peer: "172.18.0.1:5555", want: "172.18.0.1"},
		{name: "only trusted hops", peer: "172.18.0.1:5555", xff: []string{"10.0.0.7"}, want: "172.18.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.RemoteAddr = tc.peer
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if got := httpapi.ClientIP(req); got != tc.want {
				t.Errorf("ClientIP(peer %s, X-Forwarded-For %q) = %q, want %q", tc.peer, tc.xff, got, tc.want)
			}
		})
	}
}

func TestAuthRateLimitSeparatesClientsBehindDockerBridge(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t, withLimit(httpapi.RateLimitConfig{PerIPPerSecond: 0.001, PerIPBurst: 1}))
	login := func(client string) int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login", nil)
		req.RemoteAddr = "172.18.0.1:5555"
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := login("1.2.3.4"); got != http.StatusFound {
		t.Fatalf("first login from 1.2.3.4 = %d, want 302", got)
	}
	if got := login("1.2.3.4"); got != http.StatusTooManyRequests {
		t.Errorf("second login from 1.2.3.4 = %d, want 429", got)
	}
	if got := login("5.6.7.8"); got != http.StatusFound {
		t.Errorf("login from 5.6.7.8 behind the same bridge = %d, want 302 from its own bucket", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t)
	want := map[string]string{
		"Strict-Transport-Security": "max-age=31536000",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "no-referrer",
		"X-Frame-Options":           "DENY",
		"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'",
		"Permissions-Policy":        "camera=(), geolocation=(), microphone=()",
	}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/auth/login"},
		{http.MethodGet, "/auth/callback"},
		{http.MethodPost, "/auth/logout"},
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/repos/acme/widgets"},
	}
	for _, route := range routes {
		rec := env.do(t, route.method, route.path)
		for name, value := range want {
			if got := rec.Header().Get(name); got != value {
				t.Errorf("%s %s header %s = %q, want %q", route.method, route.path, name, got, value)
			}
		}
	}
}
