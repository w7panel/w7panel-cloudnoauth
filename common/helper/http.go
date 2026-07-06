package helper

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

func RemoteIPFromRequest(req *http.Request) (string, error) {
	if ip := firstHeaderIP(req.Header.Get("X-Real-IP")); ip != "" {
		return ip, nil
	}
	if ip := firstHeaderIP(req.Header.Get("X-Forwarded-For")); ip != "" {
		return ip, nil
	}
	return remoteAddrIP(req.RemoteAddr)
}

func firstHeaderIP(value string) string {
	for _, part := range strings.Split(value, ",") {
		ip := strings.TrimSpace(part)
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}

func remoteAddrIP(remoteAddr string) (string, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		if net.ParseIP(remoteAddr) != nil {
			return remoteAddr, nil
		}
		return "", fmt.Errorf("invalid remote address %q: %w", remoteAddr, err)
	}
	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid remote ip %q", host)
	}
	return host, nil
}

func ParseAllowedHosts(allowedHosts string, defaultAllowedHosts string) []string {
	if strings.TrimSpace(allowedHosts) == "" {
		allowedHosts = defaultAllowedHosts
	}

	hosts := make([]string, 0)
	for _, allowedHost := range strings.Split(allowedHosts, ",") {
		allowedHost = strings.TrimSpace(allowedHost)
		if allowedHost != "" {
			hosts = append(hosts, allowedHost)
		}
	}
	return hosts
}

func IsAllowedHost(host string, allowedHosts []string) bool {
	for _, allowedHost := range allowedHosts {
		if isSameHost(host, allowedHost) {
			return true
		}
	}
	return false
}

func isSameHost(host string, allowedHost string) bool {
	if strings.EqualFold(host, allowedHost) {
		return true
	}

	hostWithoutPort, _, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	return strings.EqualFold(hostWithoutPort, allowedHost)
}
