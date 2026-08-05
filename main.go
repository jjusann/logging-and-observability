package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

// requestLogger middleware for slog
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			logger.Info(
				"Served request",
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", r.RemoteAddr,
			)
		})
	}
}

type closeFunc func() error

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	var buffers []*bufio.Writer
	var closers []io.Closer
	var handlers []slog.Handler

	// ----- STDOUT/STDERR handler: text, DEBUG and above -----
	stderrBuf := bufio.NewWriterSize(os.Stderr, 8192)
	buffers = append(buffers, stderrBuf)

	stderrHandler := slog.NewTextHandler(stderrBuf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
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
			Level: slog.LevelInfo, // Only INFO and above go to the file
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

	// Initialize store with the logger
	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}

	// Initialize server with the logger (pass cancel)
	s := newServer(*st, httpPort, cancel, logger)

	var serverErr error
	go func() {
		logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d", httpPort))
		serverErr = s.start()
		if serverErr != nil {
			logger.Error(fmt.Sprintf("server error: %v", serverErr))
			s.cancel() // trigger shutdown via server's cancel
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