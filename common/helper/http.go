package helper

import (
	"net"
	"strings"
)

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
