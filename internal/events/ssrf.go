package events

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// BlockedIP reports addresses a webhook must never reach in production:
// loopback, private, link-local (cloud metadata), CGNAT, multicast, unspecified.
func BlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		if v4[0] == 0 || cgnat.Contains(v4) {
			return true
		}
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

func isLocalHost(h string) bool {
	h = strings.ToLower(h)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ValidateURL checks a webhook target at registration time. https only;
// plain http to localhost/127.0.0.1 is allowed outside production. In
// production literal private addresses and localhost are refused. DNS names
// are checked again at connect time by the delivery client.
func ValidateURL(raw string, production bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("url is invalid")
	}
	if u.User != nil {
		return errors.New("url must not contain credentials")
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
	case "http":
		if production || !isLocalHost(host) {
			return errors.New("url must be https (http is allowed only for localhost outside production)")
		}
	default:
		return errors.New("url must be https")
	}
	if production {
		if isLocalHost(host) {
			return errors.New("url must not point at localhost")
		}
		if ip := net.ParseIP(host); ip != nil && BlockedIP(ip) {
			return errors.New("url must not point at a private or link-local address")
		}
	}
	return nil
}

// NewClient returns the delivery client: 10s timeout, redirects never
// followed, no proxy, and (when guard is set) a dialer that refuses blocked
// addresses after DNS resolution, so a hostname cannot be rebound to one.
func NewClient(guard bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if guard {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if BlockedIP(net.ParseIP(host)) {
				return fmt.Errorf("blocked address %s", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
