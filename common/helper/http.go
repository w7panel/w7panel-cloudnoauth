package helper

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

func RemoteIPFromRequest(req *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		if net.ParseIP(req.RemoteAddr) != nil {
			return req.RemoteAddr, nil
		}
		return "", fmt.Errorf("invalid remote address %q: %w", req.RemoteAddr, err)
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
