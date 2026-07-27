package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"boot.dev/linko/internal/linkoerr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/lmittmann/tint"
	pkgerr "github.com/pkg/errors"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/store"
	"github.com/mattn/go-isatty"
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
	tp, err := initTracing(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize tracing: %v\n", err)
		return 1
	}

	defer func() error {
		if tp != nil {
			if err := tp(context.Background()); err != nil {
				fmt.Fprintf(os.Stderr, "Error closing tracing: %v\n", err)
			}
		}
		return nil
	}()

	logger, closingFunc, err := initializeLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	env := os.Getenv("ENV")
	hostname, err := os.Hostname()
	if err != nil {
		logger.Error("Error trying to get the hostname", "error", err)
		return 1
	}

	logger = logger.With(
		slog.String("env", env),                    //Lo saca Go y se agrega al logger en runtime luego
		slog.String("hostname", hostname),          //Lo saca Go y se agrega al logger en runtime luego
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
		tint.NewHandler(os.Stderr, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: resplaceAttr,
			NoColor:     !(isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())), //revisamos si estamos
		}), //en una terminal
	}

	logFile := os.Getenv("LINKO_LOG_FILE")

	if logFile != "" {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: resplaceAttr,
		}))
		return slog.New(slog.NewMultiHandler(handlers...)), func() error {
			if err := logger.Close(); err != nil {
				return err
			}
			return nil
		}, nil
	}
	return slog.New(slog.NewMultiHandler(handlers...)), nil, nil
}

func resplaceAttr(groups []string, a slog.Attr) slog.Attr {
	var sensitiveKeys = []string{"password", "key", "apikey", "secret", "pin", "creditcardno", "user"}

	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}

	if a.Value.Kind() == slog.KindString {
		if wb, err := url.Parse(a.Value.String()); err == nil {
			if _, ok := wb.User.Password(); ok {
				wb.User = url.UserPassword(wb.User.Username(), "REDACTED")
				return slog.String(a.Key, wb.String())
			}
		}

	}

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

func initTracing(ctx context.Context) (func(context.Context) error, error) {
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(resource.Default()))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
