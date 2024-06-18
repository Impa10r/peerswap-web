package main

import (
	"log"
	"net/http"
	"peerswap-web/cmd/psweb/config"
	"strings"
	"time"
)

type responseWriter struct {
	http.ResponseWriter
	brokenPipe bool
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(p)
	if err != nil {
		if strings.Contains(err.Error(), "stream closed") || strings.Contains(err.Error(), "broken pipe") {
			rw.brokenPipe = true
			log.Println("Detected broken pipe error")
		} else {
			log.Printf("Write error: %v", err)
		}
	}
	return n, err
}

// Middleware to check auth and retry on broken pipe
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

		for i := 0; i < 3; i++ { // Retry up to 3 times
			rw := &responseWriter{ResponseWriter: w}
			next.ServeHTTP(rw, r)
			if !rw.brokenPipe {
				return
			}
			time.Sleep(1 * time.Second) // Wait before retrying
		}

		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	})
}

func isAuthenticated(r *http.Request) bool {
	session, _ := store.Get(r, "session")
	auth, ok := session.Values["authenticated"].(bool)
	return ok && auth
}
