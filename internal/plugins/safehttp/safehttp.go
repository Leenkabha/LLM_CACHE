// Package safehttp is the only HTTP client the plugin platform uses to reach a
// developer-supplied endpoint. It defends against SSRF:
//
//   - the URL must be https, with no user info, query or fragment;
//   - the address actually dialled is checked after DNS resolution, so a name
//     that resolves to a private address (or is rebound later) is refused;
//   - loopback, private, link-local, CGNAT, multicast and unspecified ranges are
//     refused, and cloud-metadata addresses and names are refused even in
//     development mode;
//   - redirects are never followed;
//   - proxies from the environment are ignored;
//   - response bodies are read through a hard size limit.
//
// Development mode (ALLOW_INSECURE_PLUGIN_ENDPOINTS=true) additionally permits
// http, loopback, private addresses and Compose service names.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrTooLarge is returned when a response exceeds the configured limit.
var ErrTooLarge = errors.New("response exceeds the size limit")

// Policy selects how strict URL validation is.
type Policy struct {
	// AllowInsecure permits http, loopback, private addresses and internal
	// hostnames. Cloud-metadata targets stay blocked.
	AllowInsecure bool
}

var metadataHosts = map[string]bool{
	"metadata.google.internal": true, "metadata": true, "instance-data": true,
	"metadata.goog": true, "kubernetes.default.svc": true,
}

var (
	metadataAddrs = []netip.Addr{
		netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("169.254.170.2"),
		netip.MustParseAddr("100.100.100.200"),
		netip.MustParseAddr("fd00:ec2::254"),
	}
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
)

// ValidateURL checks a plugin endpoint URL without touching the network.
func ValidateURL(raw string, p Policy) (*url.URL, error) {
	if len(raw) > 2048 {
		return nil, errors.New("endpoint URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("endpoint must be an absolute URL such as https://plugin.example.com")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecure {
			return nil, errors.New("endpoint must use https (plain http is only allowed when ALLOW_INSECURE_PLUGIN_ENDPOINTS=true)")
		}
	default:
		return nil, fmt.Errorf("endpoint scheme %q is not allowed", u.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("endpoint must not contain credentials; supply them as a secret")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("endpoint must not contain a query string; supply credentials as a secret")
	}
	if u.Fragment != "" {
		return nil, errors.New("endpoint must not contain a fragment")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, errors.New("endpoint has no host")
	}
	if metadataHosts[host] {
		return nil, errors.New("cloud metadata endpoints are not allowed")
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if err := checkAddr(ip, p); err != nil {
			return nil, err
		}
	} else if !p.AllowInsecure {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
			strings.HasSuffix(host, ".internal") || !strings.Contains(host, ".") {
			return nil, errors.New("internal hostnames are not allowed (set ALLOW_INSECURE_PLUGIN_ENDPOINTS=true for local development)")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// checkAddr rejects a resolved address the policy does not allow.
func checkAddr(ip netip.Addr, p Policy) error {
	ip = ip.Unmap()
	for _, m := range metadataAddrs {
		if ip == m {
			return errors.New("cloud metadata addresses are not allowed")
		}
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("address %s is not allowed", ip)
	}
	if p.AllowInsecure {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip) {
		return fmt.Errorf("address %s is private or loopback; set ALLOW_INSECURE_PLUGIN_ENDPOINTS=true for local development", ip)
	}
	return nil
}

// NewClient returns an http.Client that enforces the policy at dial time, never
// follows redirects and ignores proxy environment variables.
func NewClient(p Policy, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("unparseable dial address %q", address)
			}
			return checkAddr(ip, p)
		},
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ReadLimited reads at most max bytes and fails with ErrTooLarge if the body is
// longer.
func ReadLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrTooLarge
	}
	return b, nil
}

// IsContextErr reports whether err came from a canceled or expired context.
func IsContextErr(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
