package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"boot.dev/linko/internal/store"
	"golang.org/x/crypto/bcrypt"
	pkgerr "github.com/pkg/errors"
)

const shortURLLen = len("http://localhost:8080/") + 6

var (
	redirectsMu sync.Mutex
	redirects   []string
)

//go:embed index.html
var indexPage string

func (s *server) handlerIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, indexPage)
}

func (s *server) handlerLogin(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlerShortenLink(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(UserContextKey).(string)
	if !ok || user == "" {
		httpError(r.Context(), w, http.StatusUnauthorized, pkgerr.New("unauthorized"))
		return
	}
	longURL := r.FormValue("url")
	if longURL == "" {
		httpError(r.Context(), w, http.StatusBadRequest, pkgerr.New("missing url parameter"))
		return
	}
	u, err := url.Parse(longURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		httpError(r.Context(), w, http.StatusBadRequest, pkgerr.New("invalid URL: must include scheme (http/https) and host"))
		return
	}
	if err := checkDestination(longURL); err != nil {
		httpError(r.Context(), w, http.StatusBadRequest, pkgerr.Wrap(err, "invalid target URL"))
		return
	}
	shortCode, err := s.store.Create(r.Context(), longURL)
	if err != nil {
		s.logger.Error(
			"failed to create short URL",
			"user", user,
			"url", longURL,
			"error", err,
		)
		httpError(r.Context(), w, http.StatusInternalServerError, pkgerr.New("failed to shorten URL"))
		return
	}
	s.logger.Info(
		"Successfully generated short code",
		"short_code", shortCode,
		"url", longURL,
		"user", user,
	)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, shortCode)
}

func (s *server) handlerRedirect(w http.ResponseWriter, r *http.Request) {
	shortCode := r.PathValue("shortCode")
	longURL, err := s.store.Lookup(r.Context(), shortCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.logger.Info(
				"short code not found",
				"short_code", shortCode,
			)
			httpError(r.Context(), w, http.StatusNotFound, pkgerr.New("not found"))
		} else {
			s.logger.Error(
				"failed to lookup URL",
				"short_code", shortCode,
				"error", err,
			)
			httpError(r.Context(), w, http.StatusInternalServerError, pkgerr.New("internal server error"))
		}
		return
	}
	_, _ = bcrypt.GenerateFromPassword([]byte(longURL), bcrypt.DefaultCost)
	if err := checkDestination(longURL); err != nil {
		s.logger.Warn(
			"destination unavailable",
			"url", longURL,
			"error", err,
		)
		httpError(r.Context(), w, http.StatusBadGateway, pkgerr.New("destination unavailable"))
		return
	}

	redirectsMu.Lock()
	redirects = append(redirects, strings.Repeat(longURL, 1024))
	redirectsMu.Unlock()

	s.logger.Info(
		"redirecting",
		"short_code", shortCode,
		"destination", longURL,
		"client_ip", r.RemoteAddr,
	)
	http.Redirect(w, r, longURL, http.StatusFound)
}

func (s *server) handlerListURLs(w http.ResponseWriter, r *http.Request) {
	codes, err := s.store.List(r.Context())
	if err != nil {
		s.logger.Error(
			"failed to list URLs",
			"error", err,
		)
		httpError(r.Context(), w, http.StatusInternalServerError, pkgerr.New("failed to list URLs"))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes)
}

func (s *server) handlerStats(w http.ResponseWriter, _ *http.Request) {
	redirectsMu.Lock()
	snapshot := redirects
	redirectsMu.Unlock()

	var bytesSaved int
	for _, u := range snapshot {
		bytesSaved += len(u) - shortURLLen
	}

	s.logger.Info(
		"stats requested",
		"redirects", len(snapshot),
		"bytes_saved", bytesSaved,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"redirects":   len(snapshot),
		"bytes_saved": bytesSaved,
	})
}