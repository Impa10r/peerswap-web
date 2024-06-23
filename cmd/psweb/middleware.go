package main

import (
	"net/http"
	"peerswap-web/cmd/psweb/config"
	"strings"
)

// Middleware to check authentication
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config.Config.SecureConnection && !strings.HasPrefix(r.RequestURI, "/downloadca") {
			if r.TLS != nil {
				// Check client certificate
				if len(r.TLS.PeerCertificates) == 0 {
					if config.Config.Password != "" {
						if !isAuthenticated(r) {
							if !strings.HasPrefix(r.RequestURI, "/static/") && !strings.HasPrefix(r.RequestURI, "/login") {
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

func isAuthenticated(r *http.Request) bool {
	session, _ := store.Get(r, "session")
	auth, ok := session.Values["authenticated"].(bool)
	return ok && auth
}
