package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
)

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
	if logCtx, ok := ctx.Value(LogContextKey).(*LogContext); ok && err != nil {
		logCtx.Error = err
	}
	http.Error(w, strings.ToLower(http.StatusText(status)), status)
}

type multiError interface {
	error
	Unwrap() []error
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
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

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("message", err.Error()),
	}
	if extra := linkoerr.Attrs(err); len(extra) > 0 {
		for i := 0; i < len(extra); i += 2 {
			if i+1 < len(extra) {
				key, ok := extra[i].(string)
				if !ok {
					continue
				}
				attrs = append(attrs, slog.Any(key, extra[i+1]))
			}
		}
	}
	if stacker, ok := err.(interface{ StackTrace() pkgerr.StackTrace }); ok {
		attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", stacker.StackTrace())))
	}
	return attrs
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
				"client_ip", r.RemoteAddr,
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

type closeFunc func() error

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	var buffers []*bufio.Writer
	var closers []io.Closer
	var handlers []slog.Handler

	/* replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == "error" {
			if err, ok := a.Value.Any().(error); ok {
				if multiErr, ok := err.(multiError); ok {
					subErrs := multiErr.Unwrap()
					groupedAttrs := []slog.Attr{}
					for i, subErr := range subErrs {
						if subErr != nil {
							key := fmt.Sprintf("error_%d", i+1)
							subAttrs := errorAttrs(subErr)
							groupedAttrs = append(groupedAttrs, slog.GroupAttrs(key, subAttrs...))
						}
					}
					return slog.GroupAttrs("errors", groupedAttrs...)
				}
				attrs := errorAttrs(err)
				return slog.GroupAttrs("error", attrs...)
			}
		}
		return a
	} */

	// ----- STDOUT/STDERR handler: COLORED text, DEBUG and above -----
	stderrBuf := bufio.NewWriterSize(os.Stderr, 8192)
	buffers = append(buffers, stderrBuf)

	// Check if we're in a TTY
	isTTY := isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())

	stderrHandler := tint.NewHandler(stderrBuf, &tint.Options{
		Level:   slog.LevelDebug,
		NoColor: !isTTY, // Disable colors if not a TTY
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Keep the same replaceAttr logic (error grouping, stack traces)
			if a.Key == "error" {
				if err, ok := a.Value.Any().(error); ok {
					if multiErr, ok := err.(multiError); ok {
						subErrs := multiErr.Unwrap()
						groupedAttrs := []slog.Attr{}
						for i, subErr := range subErrs {
							if subErr != nil {
								key := fmt.Sprintf("error_%d", i+1)
								subAttrs := errorAttrs(subErr)
								groupedAttrs = append(groupedAttrs, slog.GroupAttrs(key, subAttrs...))
							}
						}
						return slog.GroupAttrs("errors", groupedAttrs...)
					}
					attrs := errorAttrs(err)
					return slog.GroupAttrs("error", attrs...)
				}
			}
			return a
		},
	})
	handlers = append(handlers, stderrHandler)

	// ----- FILE handler: JSON, INFO and above -----
	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open log file: %w", err)
		}
		fileBuf := bufio.NewWriterSize(file, 8192)
		buffers = append(buffers, fileBuf)
		closers = append(closers, file)

		fileHandler := slog.NewJSONHandler(fileBuf, &slog.HandlerOptions{
			Level: slog.LevelInfo,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// Reuse the same replaceAttr for JSON logs (no colors)
				if a.Key == "error" {
					if err, ok := a.Value.Any().(error); ok {
						if multiErr, ok := err.(multiError); ok {
							subErrs := multiErr.Unwrap()
							groupedAttrs := []slog.Attr{}
							for i, subErr := range subErrs {
								if subErr != nil {
									key := fmt.Sprintf("error_%d", i+1)
									subAttrs := errorAttrs(subErr)
									groupedAttrs = append(groupedAttrs, slog.GroupAttrs(key, subAttrs...))
								}
							}
							return slog.GroupAttrs("errors", groupedAttrs...)
						}
						attrs := errorAttrs(err)
						return slog.GroupAttrs("error", attrs...)
					}
				}
				return a
			},
		})
		handlers = append(handlers, fileHandler)
	}

	// Combine all handlers into one
	multiHandler := slog.NewMultiHandler(handlers...)
	logger := slog.New(multiHandler)

	// Close function: flush all buffers and close files
	closeFn := func() error {
		for _, buf := range buffers {
			if err := buf.Flush(); err != nil {
				return fmt.Errorf("buffer flush: %w", err)
			}
		}
		for _, c := range closers {
			if err := c.Close(); err != nil {
				return fmt.Errorf("file close: %w", err)
			}
		}
		return nil
	}

	env := os.Getenv("ENV")
	if env == "" {
		env = "development"
	}
	hostname, _ := os.Hostname()

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)
	return logger, closeFn, nil
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closeFn, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := closeFn(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close logger: %v\n", err)
		}
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}

	s := newServer(*st, httpPort, cancel, logger)

	var serverErr error
	go func() {
		logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d", httpPort))
		serverErr = s.start()
		if serverErr != nil {
			logger.Error(fmt.Sprintf("server error: %v", serverErr))
			s.cancel()
		}
	}()

	<-ctx.Done()
	logger.Debug("Linko is shutting down")

	shutdownCtx, cancelTimeout := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTimeout()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		return 1
	}
	return 0
}