package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/linkoerr"

	pkgerr "github.com/pkg/errors"

	"boot.dev/linko/internal/build"
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
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),       //Gets this information at runtime to added to the logs
		slog.String("build_time", build.BuildTime), //Gets this information at runtime to added to the logs
	)

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
		logger.Error("failed to create store", "error", err)
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
			Level:       slog.LevelDebug,
			ReplaceAttr: resplaceAttr,
		}),
	}
	if os.Getenv(("LINKO_LOG_FILE")) != "" {
		file, err := os.OpenFile("linko.access.log", os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return nil, nil, err
		}
		bufferFile := bufio.NewWriterSize(file, 8192)
		handlers = append(handlers, slog.NewJSONHandler(bufferFile, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: resplaceAttr,
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

func resplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		var attrs []slog.Attr
		if multiERR, ok := errors.AsType[multiError](err); ok {
			for i, me := range multiERR.Unwrap() {
				errSlice := errorAttrs(me)
				attrs = append(attrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errSlice...))
			}
			return slog.GroupAttrs("errors", attrs...)
		} else {
			attrs = append(attrs, errorAttrs(err)...)
		}
		return slog.GroupAttrs("error", attrs...)
	}
	return a
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		{
			Key:   "message",
			Value: slog.StringValue(err.Error()),
		},
	}
	attrs = append(attrs, linkoerr.Attrs(err)...)

	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		})
	}
	return attrs
}
