package main

import (
	"net/http"
	"path"
	"peerswap-web/cmd/psweb/config"
	"strings"
)

// Middleware to check authentication
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Normalize the path: decoded, and with ./.. resolved, so prefix
		// checks can't be bypassed via traversal or trailing garbage.
		cleanPath := path.Clean(r.URL.Path)

		if config.Config.SecureConnection && cleanPath != "/downloadca" {
			if r.TLS != nil {
				// Check client certificate
				if len(r.TLS.PeerCertificates) == 0 {
					if config.Config.Password != "" {
						if !isAuthenticated(r) {
							if !isExemptPath(cleanPath) {
								http.Redirect(w, r, "/login", http.StatusFound)
								return
							}
						}
					} else {
						http.Error(w, "Client certificate not provided", http.StatusForbidden)
						return
					}
				}
			} else {
				http.Error(w, "Requires TLS connection", http.StatusForbidden)
				return
			}
		}

		// proceed
		next.ServeHTTP(w, r)
	})
}

// isExemptPath reports whether the cleaned request path is allowed
// without authentication (the login page itself and static assets).
func isExemptPath(cleanPath string) bool {
	if cleanPath == "/login" {
		return true
	}
	if cleanPath == "/static" || strings.HasPrefix(cleanPath, "/static/") {
		return true
	}
	return false
}

func isAuthenticated(r *http.Request) bool {
	session, _ := store.Get(r, "session")
	auth, ok := session.Values["authenticated"].(bool)
	return ok && auth
}
