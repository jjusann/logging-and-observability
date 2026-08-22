package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"strings"
	"time"
	"net"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
)


func redactIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present — treat the whole string as the host.
		host = addr
	}

	// Treat the IPv6 loopback as IPv4 loopback for local testing.
	if host == "::1" {
		host = "127.0.0.1"
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return addr
	}

	ip4 := ip.To4()
	if ip4 == nil {
		// Not an IPv4 address — return unchanged.
		return addr
	}

	parts := strings.Split(ip4.String(), ".")
	parts[3] = "x"
	return strings.Join(parts, ".")
}

// Helpers
func generateRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type contextKey string

const (
	UserContextKey contextKey = "user"
	LogContextKey  contextKey = "log_context"
)

type LogContext struct {
	Username  string
	Error     error
	RequestID string
}

// Spies
type spyResponseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (s *spyResponseWriter) WriteHeader(code int) {
	s.statusCode = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *spyResponseWriter) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytesWritten += n
	return n, err
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (s *spyReadCloser) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	s.bytesRead += n
	return n, err
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
    // Store the appropriate error in LogContext.
    if logCtx, ok := ctx.Value(LogContextKey).(*LogContext); ok && err != nil {
        // For 401, 403, 500, use the lowercase generic status text as the error message.
        // The original error is already logged separately in handlers.
        if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusInternalServerError {
            logCtx.Error = pkgerr.New(strings.ToLower(http.StatusText(status)))
        } else {
            logCtx.Error = err
        }
    }

    // Send the same message to the client.
    var msg string
    switch status {
    case http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError:
        msg = http.StatusText(status) // capitalized, e.g. "Unauthorized"
    default:
        if err != nil {
            msg = err.Error()
        } else {
            msg = http.StatusText(status)
        }
    }
    http.Error(w, msg, status)
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), LogContextKey, &LogContext{RequestID: requestID})
		r = r.WithContext(ctx)
		next.ServeHTTP(w, r)
	})
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			logCtx, ok := r.Context().Value(LogContextKey).(*LogContext)
			if !ok {
				logCtx = &LogContext{}
			}
			sw := &spyResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			var bodyReader *spyReadCloser
			if r.Body != nil {
				bodyReader = &spyReadCloser{ReadCloser: r.Body}
				r.Body = bodyReader
			}
			next.ServeHTTP(sw, r)
			duration := time.Since(start)
			requestBodyBytes := 0
			if bodyReader != nil {
				requestBodyBytes = bodyReader.bytesRead
			}
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", redactIP(r.RemoteAddr),
				"duration", duration.String(),
				"request_body_bytes", requestBodyBytes,
				"response_status", sw.statusCode,
				"response_body_bytes", sw.bytesWritten,
			}
			if logCtx.RequestID != "" {
				attrs = append(attrs, "request_id", logCtx.RequestID)
			}
			if logCtx.Username != "" {
				attrs = append(attrs, "user", logCtx.Username)
			}
			if logCtx.Error != nil {
				attrs = append(attrs, "error", logCtx.Error)
			}
			logger.Info("Served request", attrs...)
		})
	}
}

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
	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close logger: %v\n", err)
		}
	}()

	hostname, _ := os.Hostname()
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", os.Getenv("ENV")),
		slog.String("hostname", hostname),
	)

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
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
	logger.Debug("Linko is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}

type closeFunc func() error

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	handlers := []slog.Handler{
		tint.NewTextHandler(os.Stderr, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
			NoColor:     !(isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())),
		}),
	}

	closers := []closeFunc{}

	if logFile != "" {
		rotator := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1, // 1 MB for testing; set to 10 in production
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		handlers = append(handlers, slog.NewJSONHandler(rotator, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		}))
		closers = append(closers, func() error {
			if err := rotator.Close(); err != nil {
				return fmt.Errorf("rotator close: %w", err)
			}
			return nil
		})
	}

	closer := func() error {
		var errs []error
		for _, c := range closers {
			if err := c(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	logger := slog.New(slog.NewMultiHandler(handlers...))
	return logger, closer, nil
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
		{Key: "message", Value: slog.StringValue(err.Error())},
	}
	extra := linkoerr.Attrs(err)
	for i := 0; i < len(extra); i += 2 {
		if i+1 < len(extra) {
			key, ok := extra[i].(string)
			if !ok {
				continue
			}
			attrs = append(attrs, slog.Any(key, extra[i+1]))
		}
	}
	if stackErr, ok := err.(stackTracer); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		})
	}
	return attrs
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		if multiErr, ok := err.(multiError); ok {
			var errAttrs []slog.Attr
			for i, e := range multiErr.Unwrap() {
				errAttrs = append(errAttrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errorAttrs(e)...))
			}
			return slog.GroupAttrs("errors", errAttrs...)
		}
		return slog.GroupAttrs("error", errorAttrs(err)...)
	}
	return a
}