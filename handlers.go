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

// ---- handlers with spans ----

func (s *server) handlerIndex(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), "handler.index")
	defer span.End()

	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, indexPage)
}

func (s *server) handlerLogin(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), "handler.login")
	defer span.End()

	w.WriteHeader(http.StatusOK)
}

func (s *server) handlerShortenLink(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.shorten_link")
	defer span.End()

	user, ok := ctx.Value(UserContextKey).(string)
	if !ok || user == "" {
		httpError(ctx, w, http.StatusUnauthorized, pkgerr.New("unauthorized"))
		return
	}
	longURL := r.FormValue("url")
	if longURL == "" {
		httpError(ctx, w, http.StatusBadRequest, pkgerr.New("missing url parameter"))
		return
	}
	u, err := url.Parse(longURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		httpError(ctx, w, http.StatusBadRequest, pkgerr.New("invalid URL: must include scheme (http/https) and host"))
		return
	}
	if err := checkDestination(ctx, longURL); err != nil {
		httpError(ctx, w, http.StatusBadRequest, pkgerr.Wrap(err, "invalid target URL"))
		return
	}
	shortCode, err := s.store.Create(ctx, longURL)
	if err != nil {
		s.logger.Error(
			"failed to create short URL",
			"user", user,
			"long_url", longURL,
			"error", err,
		)
		httpError(ctx, w, http.StatusInternalServerError, pkgerr.New("failed to shorten URL"))
		return
	}
	s.logger.Info(
		"Successfully generated short code",
		"short_code", shortCode,
		"long_url", longURL,
		"user", user,
	)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, shortCode)
}

func (s *server) handlerRedirect(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.redirect")
	defer span.End()

	shortCode := r.PathValue("shortCode")
	longURL, err := s.store.Lookup(ctx, shortCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.logger.Info(
				"short code not found",
				"short_code", shortCode,
			)
			httpError(ctx, w, http.StatusNotFound, pkgerr.New("not found"))
		} else {
			s.logger.Error(
				"failed to lookup URL",
				"short_code", shortCode,
				"error", err,
			)
			httpError(ctx, w, http.StatusInternalServerError, pkgerr.New("internal server error"))
		}
		return
	}
	_, _ = bcrypt.GenerateFromPassword([]byte(longURL), bcrypt.DefaultCost)
	if err := checkDestination(ctx, longURL); err != nil {
		s.logger.Warn(
			"destination unavailable",
			"long_url", longURL,
			"error", err,
		)
		httpError(ctx, w, http.StatusBadGateway, pkgerr.New("destination unavailable"))
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
	ctx, span := tracer.Start(r.Context(), "handler.list_urls")
	defer span.End()

	codes, err := s.store.List(ctx)
	if err != nil {
		s.logger.Error(
			"failed to list URLs",
			"error", err,
		)
		httpError(ctx, w, http.StatusInternalServerError, pkgerr.New("failed to list URLs"))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes)
}

func (s *server) handlerStats(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), "handler.stats")
	defer span.End()

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