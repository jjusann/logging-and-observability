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
		ctx := r.Context()
		username, password, ok := r.BasicAuth()
		if !ok {
			httpError(ctx, w, http.StatusUnauthorized, pkgerr.New("basic auth required"))
			return
		}
		stored, exists := allowedUsers[username]
		if !exists {
			s.logger.Info(
				"login attempt with unknown user",
				"user", username,
				"client_ip", r.RemoteAddr,
			)
			httpError(ctx, w, http.StatusUnauthorized, pkgerr.New("invalid credentials"))
			return
		}
		ok, err := s.validatePassword(ctx, username, password, stored)
		if err != nil {
			s.logger.Error(
				"error validating password",
				"user", username,
				"error", err,
			)
			httpError(ctx, w, http.StatusInternalServerError, pkgerr.New("internal server error"))
			return
		}
		if !ok {
			s.logger.Info(
				"invalid credentials",
				"user", username,
				"client_ip", r.RemoteAddr,
			)
			httpError(ctx, w, http.StatusUnauthorized, pkgerr.New("invalid credentials"))
			return
		}

		// Set username in log context
		if logCtx, ok := ctx.Value(LogContextKey).(*LogContext); ok {
			logCtx.Username = username
		}

		s.logger.Info(
			"user authenticated",
			"user", username,
			"client_ip", r.RemoteAddr,
		)
		r = r.WithContext(context.WithValue(ctx, UserContextKey, username))
		next.ServeHTTP(w, r)
	})
}

// validatePassword now takes a context and creates a child span.
func (s *server) validatePassword(ctx context.Context, username, password, stored string) (bool, error) {
	_, span := tracer.Start(ctx, "auth.validate_password")
	defer span.End()

	err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	if err != nil {
		return false, pkgerr.WithStack(err)
	}
	return true, nil
}