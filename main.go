package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

type closeFunction func() error

func main() {

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closingFunc, err := initializeLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() error {
		if closingFunc != nil {
			if err := closingFunc(); err != nil {
				fmt.Fprintf(os.Stderr, "Error al intentar cerrar el archivo: %v\n", err)
			}
		}
		return nil
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error("failed to create store", slog.String("error", err.Error()))
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("Failed to shutdown server", slog.String("error", err.Error()))
		return 1
	}
	if serverErr != nil {
		logger.Error("Server error", slog.String("error", serverErr.Error()))
		return 1
	}
	return 0
}

func initializeLogger() (*slog.Logger, closeFunction, error) {
	handlers := []slog.Handler{
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}),
	}
	if os.Getenv(("LINKO_LOG_FILE")) != "" {
		file, err := os.OpenFile("linko.access.log", os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return nil, nil, err
		}
		bufferFile := bufio.NewWriterSize(file, 8192)
		handlers = append(handlers, slog.NewTextHandler(bufferFile, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
		return slog.New(slog.NewMultiHandler(handlers...)), func() error {
			if err := bufferFile.Flush(); err != nil {
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			return nil
		}, nil
	}
	return slog.New(slog.NewMultiHandler(handlers...)), nil, nil
}
