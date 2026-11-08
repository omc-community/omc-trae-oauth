package main

import (
	"net/url"
	"strings"
)

// traeDomains are the domains a sign-in may name a host on.
var traeDomains = []string{"trae.cn", "trae.ai", "mchost.guru"}

// traeHost is a host a sign-in named, when it is a well-formed https one on
// Trae's own domains (no port, no credentials), as origin plus any path; ""
// for anything else, which is then not taken.
func traeHost(raw string) string {
	u, err := parseHost(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	n := strings.ToLower(u.Hostname())
	ok := false
	for _, d := range traeDomains {
		if n == d || strings.HasSuffix(n, "."+d) {
			ok = true
			break
		}
	}
	if !ok {
		return ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
}

func parseHost(raw string) (*url.URL, error) {
	return url.Parse(strings.TrimSpace(raw))
}

// isTraeCNModelHost reports a host that serves Trae CN's models: the consumer
// gateway (mchost.guru, or a trae-api-… host), or another trae.cn host than the
// consumer site's own (www, api, grow…), which an enterprise sign-in may name
// for its tenant.
func isTraeCNModelHost(h string) bool {
	u, err := parseHost(h)
	if err != nil {
		return false
	}
	n := strings.ToLower(u.Hostname())
	if strings.HasSuffix(n, "mchost.guru") || strings.HasPrefix(n, "trae-api-") {
		return true
	}
	return strings.HasSuffix(n, ".trae.cn") && !consumerTraeCNHost(n)
}

func consumerTraeCNHost(n string) bool {
	return n == "www.trae.cn" || n == "api.trae.cn" || strings.HasPrefix(n, "grow")
}

// isTraeGlobalModelHost reports a host on trae.ai that is not the consumer
// site's own (www, api-…, grow…).
func isTraeGlobalModelHost(h string) bool {
	u, err := parseHost(h)
	if err != nil {
		return false
	}
	n := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(n, ".trae.ai") {
		return false
	}
	return n != "www.trae.ai" && !strings.HasPrefix(n, "api-") && !strings.HasPrefix(n, "grow")
}

// isTraeGlobalUSHost reports the US deployment's model or usage host.
func isTraeGlobalUSHost(h string) bool {
	u, err := parseHost(h)
	if err != nil {
		return false
	}
	n := strings.ToLower(u.Hostname())
	return strings.Contains(n, "coreva") || strings.HasPrefix(n, "api-us-")
}

// isUSRegion reports the international deployment's US region, as the
// sign-in's AIRegion names it.
func isUSRegion(region string) bool {
	r := strings.ToUpper(strings.TrimSpace(region))
	return r == "US" || strings.HasPrefix(r, "US-") || strings.HasPrefix(r, "US_") || strings.HasPrefix(r, "US ")
}
