package main

import (
	"context"
	"net/http"

	pkgerr "github.com/pkg/errors"
	"golang.org/x/crypto/bcrypt"
)

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
            httpError(r.Context(), w, http.StatusUnauthorized, pkgerr.New("basic auth required"))
            return
        }
        stored, exists := allowedUsers[username]
        if !exists {
            s.logger.Info(
                "login attempt with unknown user",
                "user", username,
                "client_ip", r.RemoteAddr,
            )
            httpError(r.Context(), w, http.StatusUnauthorized, pkgerr.New("invalid credentials"))
            return
        }
        ok, err := s.validatePassword(username, password, stored)
        if err != nil {
            s.logger.Error(
                "error validating password",
                "user", username,
                "error", err,
            )
            httpError(r.Context(), w, http.StatusInternalServerError, pkgerr.New("internal server error"))
            return
        }
        if !ok {
            s.logger.Info(
                "invalid credentials",
                "user", username,
                "client_ip", r.RemoteAddr,
            )
            httpError(r.Context(), w, http.StatusUnauthorized, pkgerr.New("invalid credentials"))
            return
        }

        // ✅ Set username in log context
        if logCtx, ok := r.Context().Value(LogContextKey).(*LogContext); ok {
            logCtx.Username = username
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
		return false, pkgerr.WithStack(err)
	}
	return true, nil
}