package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
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
		if err := closingFunc(); err != nil {
			fmt.Fprintf(os.Stderr, "Error al intentar cerrar el archivo: %v", err)
		}
		return nil
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Printf("failed to create store: %v", err)
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
		logger.Printf("failed to shutdown server: %v", err)
		return 1
	}
	if serverErr != nil {
		logger.Printf("server error: %v", serverErr)
		return 1
	}
	return 0
}

func initializeLogger() (*log.Logger, closeFunction, error) {
	if os.Getenv(("LINKO_LOG_FILE")) != "" {
		file, err := os.OpenFile("linko.access.log", os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return nil, nil, err
		}
		bufferFile := bufio.NewWriterSize(file, 8192)
		multilogger := io.MultiWriter(os.Stderr, bufferFile)
		return log.New(multilogger, "", log.LstdFlags), func() error {
			if err := bufferFile.Flush(); err != nil {
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			return nil
		}, nil
	}
	return log.New(os.Stderr, "", log.LstdFlags), nil, nil
}
