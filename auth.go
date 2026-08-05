package main

import (
	"context"
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const UserContextKey contextKey = "user"

var allowedUsers = map[string]string{
	"frodo":   "$2a$10$B6O/n6teuCzpuh66jrUAdeaJ3WvXcxRkzpN0x7H.di9G9e/NGb9Me",
	"samwise": "$2a$10$EWZpvYhUJtJcEMmm/IBOsOGIcpxUnGIVMRiDlN/nxl1RRwWGkJtty",
	// frodo: "ofTheNineFingers"
	// samwise: "theStrong"
	"saruman": "invalidFormat",
}

func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		stored, exists := allowedUsers[username]
		if !exists {
			s.logger.Info(
				"login attempt with unknown user",
				"user", username,
				"client_ip", r.RemoteAddr,
			)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		ok, err := s.validatePassword(username, password, stored)
		if err != nil {
			// Log the error once here, with context
			s.logger.Error(
				"error validating password",
				"user", username,
				"error", err,
			)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if !ok {
			s.logger.Info(
				"invalid credentials",
				"user", username,
				"client_ip", r.RemoteAddr,
			)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		s.logger.Info(
			"user authenticated",
			"user", username,
			"client_ip", r.RemoteAddr,
		)
		r = r.WithContext(context.WithValue(r.Context(), UserContextKey, username))
		next.ServeHTTP(w, r)
	})
}

func (s *server) validatePassword(username, password, stored string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	if err != nil {
		// Log is removed here – it's handled in the middleware with user context
		return false, err
	}
	return true, nil
}