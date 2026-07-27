package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"boot.dev/linko/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	shortURLLen        = len("http://localhost:8080/") + 6
	handlerIndex       = "handler.index"
	handlerLogin       = "handler.login"
	handlerShortenLink = "handler.shorten_link"
)

var (
	redirectsMu sync.Mutex
	redirects   []string
)

//go:embed index.html
var indexPage string

func (s *server) handlerIndex(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), handlerIndex)
	defer span.End()
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, indexPage)
}

func (s *server) handlerLogin(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), handlerLogin)
	defer span.End()
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlerShortenLink(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), handlerShortenLink)
	defer span.End()
	user, ok := r.Context().Value(UserContextKey).(string)
	if !ok || user == "" {
		httpError(ctx, w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	longURL := r.FormValue("url")
	if longURL == "" {
		httpError(ctx, w, fmt.Errorf("missing url parameter"), http.StatusBadRequest)
		return
	}
	u, err := url.Parse(longURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		httpError(ctx, w, fmt.Errorf("invalid URL: must include scheme (http/https) and host: %w", err), http.StatusBadRequest)
		return
	}
	if err := checkDestination(ctx, longURL); err != nil {
		httpError(ctx, w, fmt.Errorf("invalid target URL: %w", err), http.StatusBadRequest)
		return
	}
	shortCode, err := s.store.Create(ctx, longURL)
	if err != nil {
		httpError(ctx, w, fmt.Errorf("failed to shorten URL: %w", err), http.StatusInternalServerError)
		return
	}
	s.logger.Info("Successfully generated short code", slog.String("shortCode", shortCode), slog.String("long_url", longURL))
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, shortCode)
}

func (s *server) handlerRedirect(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.redirect")
	defer span.End()
	longURL, err := s.store.Lookup(ctx, r.PathValue("shortCode"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(ctx, w, fmt.Errorf("not found"), http.StatusNotFound)
		} else {
			s.logger.Error("failed to lookup URL", slog.String("url", longURL), "error", err)
			httpError(ctx, w, fmt.Errorf("internal server error: %w", err), http.StatusInternalServerError)
		}
		return
	}
	_, _ = bcrypt.GenerateFromPassword([]byte(longURL), bcrypt.DefaultCost)
	if err := checkDestination(ctx, longURL); err != nil {
		httpError(ctx, w, fmt.Errorf("destination unavailable: %w", err), http.StatusBadGateway)
		return
	}

	redirectsMu.Lock()
	redirects = append(redirects, strings.Repeat(longURL, 1024))
	redirectsMu.Unlock()

	http.Redirect(w, r, longURL, http.StatusFound)
}

func (s *server) handlerListURLs(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.redirect")
	defer span.End()
	codes, err := s.store.List(ctx)
	if err != nil {
		s.logger.Error("failed to list URLs", "error", err)
		httpError(ctx, w, fmt.Errorf("failed to list URLs: %w", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes)
}

func (s *server) handlerStats(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), "handler.redirect")
	defer span.End()
	redirectsMu.Lock()
	snapshot := redirects
	redirectsMu.Unlock()

	var bytesSaved int
	for _, u := range snapshot {
		bytesSaved += len(u) - shortURLLen
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"redirects":   len(snapshot),
		"bytes_saved": bytesSaved,
	})
}
