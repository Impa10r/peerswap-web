package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// Middleware to retry on broken pipe
func retryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ { // Retry up to 3 times
			rw := &responseWriter{w, false}
			next.ServeHTTP(rw, r)
			if !rw.brokenPipe {
				return
			}
			log.Println("Retrying due to broken pipe...")
			time.Sleep(1 * time.Second) // Wait before retrying
		}
	})
}

// Custom ResponseWriter to detect broken pipe
type responseWriter struct {
	http.ResponseWriter
	brokenPipe bool
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	if err != nil {
		if isBrokenPipeError(err) {
			rw.brokenPipe = true
		}
	}
	return n, err
}

func isBrokenPipeError(err error) bool {
	if ne, ok := err.(*net.OpError); ok {
		if se, ok := ne.Err.(*os.SyscallError); ok && se.Syscall == "write" {
			if se.Err == syscall.EPIPE {
				return true
			}
		}
	}
	return false
}
