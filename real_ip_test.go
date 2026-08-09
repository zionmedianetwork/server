package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// realIPFor sends one request from the given peer address, carrying the given
// headers, and returns the address the server settled on. It checks along the
// way that the access log recorded the same address the handler saw: RealIP
// and the remote_ip field are the same decision, so a fix that separated them
// would be a bug either way round.
func realIPFor(t *testing.T, cfg *HttpConfig, peer string, headers map[string]string) string {
	t.Helper()

	s, log := newTestServerWith(t, cfg)
	s.Echo().GET("/whoami", func(c echo.Context) error {
		return c.String(http.StatusOK, c.RealIP())
	})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = net.JoinHostPort(peer, "51234")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := do(t, s, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	seen := rec.Body.String()
	wantField(t, onlyEntry(t, log), "remote_ip", seen)

	return seen
}

// spoofed is the address a caller puts in a forwarding header when it wants to
// be someone else; peer addresses in these tests are the real connection.
const spoofed = "1.2.3.4"

// TestRealIPIgnoresForwardingHeadersByDefault covers H2. With IPExtractor left
// unset echo falls back to legacy behaviour and reads X-Forwarded-For, then
// X-Real-IP, from whoever sent them — so any caller could name its own address
// and have it land in the access log, and in anything later built on RealIP.
func TestRealIPIgnoresForwardingHeadersByDefault(t *testing.T) {
	tests := []struct {
		name    string
		peer    string
		headers map[string]string
		want    string
	}{
		{
			name: "no headers is the peer",
			peer: "10.0.0.5",
			want: "10.0.0.5",
		},
		{
			name:    "spoofed x-forwarded-for is ignored",
			peer:    "10.0.0.5",
			headers: map[string]string{echo.HeaderXForwardedFor: spoofed},
			want:    "10.0.0.5",
		},
		{
			name:    "spoofed x-real-ip is ignored",
			peer:    "10.0.0.5",
			headers: map[string]string{echo.HeaderXRealIP: spoofed},
			want:    "10.0.0.5",
		},
		{
			name: "both headers are ignored",
			peer: "10.0.0.5",
			headers: map[string]string{
				echo.HeaderXForwardedFor: spoofed,
				echo.HeaderXRealIP:       spoofed,
			},
			want: "10.0.0.5",
		},
		{
			name:    "an internet peer cannot rename itself either",
			peer:    "203.0.113.9",
			headers: map[string]string{echo.HeaderXForwardedFor: "10.0.0.5"},
			want:    "203.0.113.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := realIPFor(t, testConfig(), tt.peer, tt.headers); got != tt.want {
				t.Errorf("RealIP() = %q, want the peer %q", got, tt.want)
			}
		})
	}
}

// TestRealIPFromEnvDefaultIgnoresForwardingHeaders repeats the check against a
// config nobody edited, since that is what a deployment that sets no new
// variables actually gets.
func TestRealIPFromEnvDefaultIgnoresForwardingHeaders(t *testing.T) {
	cfg, err := NewHttpConfig()
	if err != nil {
		t.Fatalf("NewHttpConfig() error = %v, want nil", err)
	}
	if cfg.RealIPSource != RealIPSourcePeer {
		t.Fatalf("RealIPSource = %q, want %q", cfg.RealIPSource, RealIPSourcePeer)
	}

	got := realIPFor(t, cfg, "10.0.0.5", map[string]string{echo.HeaderXForwardedFor: spoofed})
	if want := "10.0.0.5"; got != want {
		t.Errorf("RealIP() = %q, want the peer %q", got, want)
	}
}

// TestRealIPFromXFFTrustsInternalHops covers the opt-in mode with no ranges
// named, which is the sidecar or cluster-internal ingress case: loopback and
// the private ranges relay a client address, nothing else does.
func TestRealIPFromXFFTrustsInternalHops(t *testing.T) {
	cfg := testConfig()
	cfg.RealIPSource = RealIPSourceXFF

	tests := []struct {
		name    string
		peer    string
		headers map[string]string
		want    string
	}{
		{
			name:    "private proxy relays the client",
			peer:    "10.0.0.5",
			headers: map[string]string{echo.HeaderXForwardedFor: spoofed},
			want:    spoofed,
		},
		{
			name:    "loopback proxy relays the client",
			peer:    "127.0.0.1",
			headers: map[string]string{echo.HeaderXForwardedFor: spoofed},
			want:    spoofed,
		},
		{
			name:    "the rightmost untrusted hop wins",
			peer:    "10.0.0.5",
			headers: map[string]string{echo.HeaderXForwardedFor: spoofed + ", 192.168.1.7"},
			want:    spoofed,
		},
		{
			name:    "a client appending to the header cannot win",
			peer:    "10.0.0.5",
			headers: map[string]string{echo.HeaderXForwardedFor: "9.9.9.9, " + spoofed},
			want:    spoofed,
		},
		{
			name: "no header is the peer",
			peer: "10.0.0.5",
			want: "10.0.0.5",
		},
		{
			name:    "an untrusted peer is not believed",
			peer:    "203.0.113.9",
			headers: map[string]string{echo.HeaderXForwardedFor: spoofed},
			want:    "203.0.113.9",
		},
		{
			name:    "link local is not a proxy hop",
			peer:    "169.254.10.1",
			headers: map[string]string{echo.HeaderXForwardedFor: spoofed},
			want:    "169.254.10.1",
		},
		{
			name:    "x-real-ip is still ignored in this mode",
			peer:    "10.0.0.5",
			headers: map[string]string{echo.HeaderXRealIP: spoofed},
			want:    "10.0.0.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := realIPFor(t, cfg, tt.peer, tt.headers); got != tt.want {
				t.Errorf("RealIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRealIPFromXFFTrustsOnlyListedProxies pins the meaning of a populated
// HTTP_TRUSTED_PROXIES: the list is the whole answer. An operator who names
// the load balancer has said where the proxies are, and the built-in trust in
// every private and loopback address stands down rather than quietly widening
// what was asked for.
func TestRealIPFromXFFTrustsOnlyListedProxies(t *testing.T) {
	cfg := testConfig()
	cfg.RealIPSource = RealIPSourceXFF
	cfg.TrustedProxies = []string{"10.1.0.0/16", " 192.0.2.0/24 "}

	tests := []struct {
		name string
		peer string
		want string
	}{
		{name: "listed range relays the client", peer: "10.1.0.7", want: spoofed},
		{name: "second listed range relays the client", peer: "192.0.2.10", want: spoofed},
		{name: "unlisted private peer is not believed", peer: "10.9.0.7", want: "10.9.0.7"},
		{name: "loopback is no longer implicit", peer: "127.0.0.1", want: "127.0.0.1"},
		{name: "internet peer is not believed", peer: "203.0.113.9", want: "203.0.113.9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{echo.HeaderXForwardedFor: spoofed}
			if got := realIPFor(t, cfg, tt.peer, headers); got != tt.want {
				t.Errorf("RealIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNewHTTPRejectsInvalidRealIPConfig checks that operator input this
// package cannot act on is refused out loud. Falling back to a working default
// would leave someone believing a proxy is trusted when it is not, or the
// reverse, and neither shows up until an audit needs the addresses.
func TestNewHTTPRejectsInvalidRealIPConfig(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		proxies  []string
		wantText string
	}{
		{
			name:     "unknown source",
			source:   "trust-everyone",
			wantText: "trust-everyone",
		},
		{
			name:     "source is case sensitive",
			source:   "XFF",
			wantText: "XFF",
		},
		{
			name:     "trusted proxy is not a cidr",
			source:   RealIPSourceXFF,
			proxies:  []string{"10.1.0.7"},
			wantText: "10.1.0.7",
		},
		{
			name:     "trusted proxy is nonsense",
			source:   RealIPSourceXFF,
			proxies:  []string{"10.1.0.0/16", "not-an-address"},
			wantText: "not-an-address",
		},
		{
			name:     "a bad range is caught even where it would not be used",
			source:   RealIPSourcePeer,
			proxies:  []string{"not-an-address"},
			wantText: "not-an-address",
		},
		{
			name:     "trusted proxies with a source that never reads headers",
			source:   RealIPSourcePeer,
			proxies:  []string{"10.1.0.0/16"},
			wantText: "HTTP_REAL_IP_SOURCE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RealIPSource = tt.source
			cfg.TrustedProxies = tt.proxies

			err := newHTTPError(t, cfg)
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("NewHTTP() error = %v, want it to mention %q", err, tt.wantText)
			}
		})
	}
}

func TestNewHTTPRejectsInvalidRealIPConfigFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantText string
	}{
		{
			name:     "unknown source",
			env:      map[string]string{"HTTP_REAL_IP_SOURCE": "yes-please"},
			wantText: "yes-please",
		},
		{
			name: "malformed trusted proxy",
			env: map[string]string{
				"HTTP_REAL_IP_SOURCE":  RealIPSourceXFF,
				"HTTP_TRUSTED_PROXIES": "10.1.0.0/16,10.2.0.0",
			},
			wantText: "10.2.0.0",
		},
		{
			name:     "trusted proxies without the matching source",
			env:      map[string]string{"HTTP_TRUSTED_PROXIES": "10.1.0.0/16"},
			wantText: "HTTP_REAL_IP_SOURCE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			err := newHTTPError(t, nil)
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("NewHTTP() error = %v, want it to mention %q", err, tt.wantText)
			}
		})
	}
}

// TestTrustedProxiesIgnoresBlankEntries guards a sharp edge of envconfig:
// HTTP_TRUSTED_PROXIES set to the empty string arrives as a one-element list
// holding that empty string, so a deployment that clears the variable must not
// be read as having named a proxy.
func TestTrustedProxiesIgnoresBlankEntries(t *testing.T) {
	t.Run("blank entries in a hand-built config", func(t *testing.T) {
		cfg := testConfig()
		cfg.TrustedProxies = []string{"", "   "}

		if got := realIPFor(t, cfg, "10.0.0.5", map[string]string{echo.HeaderXForwardedFor: spoofed}); got != "10.0.0.5" {
			t.Errorf("RealIP() = %q, want the peer %q", got, "10.0.0.5")
		}
	})

	t.Run("the variable set to nothing", func(t *testing.T) {
		t.Setenv("HTTP_TRUSTED_PROXIES", "")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if _, err := NewHTTP(cfg, &stubLogger{}); err != nil {
			t.Fatalf("NewHTTP() error = %v, want nil for an empty HTTP_TRUSTED_PROXIES", err)
		}
	})
}

func TestNewHttpConfigRealIPSettings(t *testing.T) {
	t.Run("source defaults to the peer address", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.RealIPSource != RealIPSourcePeer {
			t.Errorf("RealIPSource = %q, want %q", cfg.RealIPSource, RealIPSourcePeer)
		}
		if len(cfg.TrustedProxies) != 0 {
			t.Errorf("TrustedProxies = %q, want empty by default", cfg.TrustedProxies)
		}
	})

	t.Run("reads HTTP_REAL_IP_SOURCE and HTTP_TRUSTED_PROXIES", func(t *testing.T) {
		t.Setenv("HTTP_REAL_IP_SOURCE", RealIPSourceXFF)
		t.Setenv("HTTP_TRUSTED_PROXIES", "10.1.0.0/16,192.0.2.0/24")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.RealIPSource != RealIPSourceXFF {
			t.Errorf("RealIPSource = %q, want %q", cfg.RealIPSource, RealIPSourceXFF)
		}
		if want := []string{"10.1.0.0/16", "192.0.2.0/24"}; !slices.Equal(cfg.TrustedProxies, want) {
			t.Errorf("TrustedProxies = %q, want %q", cfg.TrustedProxies, want)
		}
	})
}
