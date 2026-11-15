package relay

import "strings"

type route struct {
	method string
	parts  []string // "*" matches exactly one segment
}

func r(method, pattern string) route {
	return route{method: method, parts: strings.Split(strings.Trim(pattern, "/"), "/")}
}

// allowlist is every route a relayed request may reach (docs/hosted-protocol.md, section 8).
// Anything else is refused even with a valid signature. There is no entry for login, sessions,
// auth setup, config writes, device approval or the kill-switch reset.
var allowlist = []route{
	r("GET", "/api/workspaces"),
	r("GET", "/api/inbox"),
	r("GET", "/api/workspaces/*/inbox"),
	r("GET", "/api/workspaces/*/chat"),
	r("GET", "/api/workspaces/*/activity"),
	r("GET", "/api/workspaces/*/floors"),
	r("GET", "/api/workspaces/*/docs"),
	r("GET", "/api/floors"),
	r("GET", "/api/wire"),
	r("GET", "/api/usage"),
	r("GET", "/api/insights"),
	r("GET", "/api/stream"),
	r("GET", "/api/audit"),
	r("GET", "/api/relay/info"),
	r("GET", "/api/devices"),

	r("GET", "/"),
	r("GET", "/workspaces/*/tasks"),
	r("GET", "/workspaces/*/chat"),
	r("GET", "/workspaces/*/activity"),
	r("GET", "/workspaces/*/floors"),
	r("GET", "/workspaces/*/docs"),
	r("GET", "/workspaces/*/docs/asset"),
	r("GET", "/usage"),
	r("GET", "/insights"),

	r("POST", "/api/inbox/dismiss-notices"),
	r("POST", "/api/workspaces/*/inbox/dismiss-notices"),
	r("POST", "/api/workspaces/*/inbox/*/respond"),
	r("POST", "/api/workspaces/*/inbox/*/dismiss"),
	r("POST", "/api/workspaces/*/inbox/*/postpone"),
	r("POST", "/api/workspaces/*/chat"),
	r("POST", "/api/workspaces/*/agents/*/approve"),
	r("POST", "/api/workspaces/*/agents/*/reject"),
	r("POST", "/api/workspaces/*/agents/*/prompt"),
	r("POST", "/api/standby/config"),
	r("POST", "/api/standby/resume"),
	r("POST", "/api/devices/*/revoke"),
	r("POST", "/api/kill-switch"),
}

// Allowed reports whether a relayed request for method and path may run.
func Allowed(method, path string) bool {
	if path == "" || path[0] != '/' {
		return false
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, rt := range allowlist {
		if rt.method != method || len(rt.parts) != len(segs) {
			continue
		}
		ok := true
		for i, p := range rt.parts {
			if p != "*" && p != segs[i] {
				ok = false
				break
			}
			if p == "*" && segs[i] == "" {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// IsStream reports whether the route is the live event stream (long-lived response).
func IsStream(method, path string) bool { return method == "GET" && path == "/api/stream" }

// IsWrite reports whether a method changes state (counts double against the rate limit).
func IsWrite(method string) bool { return method != "GET" }
