package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"boot.dev/linko/internal/store"
)

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

func newServer(store store.Store, port int, cancel context.CancelFunc, logger *slog.Logger) *server {
	mux := http.NewServeMux()

	s := &server{
		store:  store,
		cancel: cancel,
		logger: logger,
	}

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: requestMiddleware(RequestLogger(logger)(mux)),
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	s.logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%v", ln.Addr().(*net.TCPAddr).Port))

	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
	s.logger.Debug("Linko is shutting down")
}

func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			spyW := &spyWrapper{ReadCloser: r.Body}
			spyR := &spyResponse{ResponseWriter: w}
			r.Body = spyW
			loggerContext := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), logContextKey, loggerContext)) //Se crea una copia de request
			next.ServeHTTP(spyR, r)                                                         //con contexto modificado al que se le agrega
			requestId := spyR.Header().Get("X-Request-ID")                                  //un puntuero a un struct
			attrs := []any{
				slog.String("method", r.Method), //unas variables en forma clave:valor
				slog.String("path", r.URL.Path),
				slog.String("client_ip", r.RemoteAddr), slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyW.bytesRead), slog.Int("response_body_bytes", spyR.responseBytes),
				slog.Int("response_status", spyR.responseStatus),
				slog.String("request_id", requestId),
			}
			if loggerContext.Username != "" {
				attrs = append(attrs, slog.String("user", loggerContext.Username))
			}
			if loggerContext.Error != nil {
				attrs = append(attrs, slog.Any("error", loggerContext.Error))
			}
			logger.Info("Served request", attrs...)
		})
	}
}

type spyWrapper struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyWrapper) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type spyResponse struct {
	http.ResponseWriter
	responseStatus int
	responseBytes  int
}

func (s *spyResponse) Write(p []byte) (int, error) {
	if s.responseStatus == 0 {
		s.responseStatus = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.responseBytes += n
	return n, err
}

func (s *spyResponse) WriteHeader(statusCode int) {
	s.responseStatus = statusCode
	s.ResponseWriter.WriteHeader(statusCode)
}

func httpError(c context.Context, w http.ResponseWriter, err error, statusC int) {
	if logCtx, ok := c.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	errorMsg := err.Error()

	if statusC == 401 || statusC == 403 || statusC == 500 {
		errorMsg = http.StatusText(statusC)
	}

	http.Error(w, errorMsg, statusC)
}

func requestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = rand.Text()
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r)
	})
}
