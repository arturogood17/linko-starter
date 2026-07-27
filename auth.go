package main

import (
	"context"
	"fmt"
	"net/http"

	pkgerr "github.com/pkg/errors"

	"golang.org/x/crypto/bcrypt"
)

type contextKey string //se crea este tipo para evitar conflictos, por ejemplo, si tuvieses varias claves con el mismo nombre

const (
	UserContextKey contextKey = "user"
	valPass                   = "auth.validate_password"
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
			httpError(r.Context(), w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
			return
		}
		stored, exists := allowedUsers[username]
		if !exists {
			httpError(r.Context(), w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
			return
		}
		ok, err := s.validatePassword(r.Context(), password, stored)
		if err != nil {
			s.logger.Info("error validating password", "user", username, "error", err)
			httpError(r.Context(), w, fmt.Errorf("internal server error: %w", err), http.StatusInternalServerError)
			return
		}
		if !ok {
			httpError(r.Context(), w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
			return
		}
		if val, ok := r.Context().Value(logContextKey).(*LogContext); ok {
			val.Username = username
		}
		r = r.WithContext(context.WithValue(r.Context(), UserContextKey, username)) //Esto se tiene que quedar
		next.ServeHTTP(w, r)                                                        //porque es parte de la request, no del logger
	})
}

func (s *server) validatePassword(ctx context.Context, password, stored string) (bool, error) {
	_, span := tracer.Start(ctx, valPass)
	span.End()
	err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	if err != nil {
		return false, pkgerr.WithStack(err)
	}
	return true, nil
}
