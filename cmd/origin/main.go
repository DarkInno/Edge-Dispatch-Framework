package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("starting origin server")

	cfg := config.LoadOrigin()

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		logger.Error("failed to create data directory", "path", cfg.DataDir, "error", err)
		os.Exit(1)
	}

	// Create sample file if directory is empty
	entries, err := os.ReadDir(cfg.DataDir)
	if err != nil {
		logger.Error("failed to read data directory", "error", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		sampleDir := filepath.Join(cfg.DataDir, "video")
		sampleFile := filepath.Join(sampleDir, "sample.mp4")
		if err := os.MkdirAll(sampleDir, 0755); err != nil {
			logger.Error("failed to create sample directory", "error", err)
			os.Exit(1)
		}
		// Generate 1MB of random data
		data := make([]byte, 1024*1024)
		if _, err := rand.Read(data); err != nil {
			logger.Error("failed to generate random data", "error", err)
			os.Exit(1)
		}
		if err := os.WriteFile(sampleFile, data, 0644); err != nil {
			logger.Error("failed to write sample file", "error", err)
			os.Exit(1)
		}
		logger.Info("created sample file", "path", sampleFile, "size_bytes", len(data))
	}

	// Setup router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)

	// Object serving endpoint with Range support
	r.Get("/obj/*", func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "*")
		if key == "" || key == "." || strings.Contains(r.URL.Path, "..") {
			http.Error(w, "invalid key", http.StatusBadRequest)
			return
		}

		filePath := filepath.Join(cfg.DataDir, key)
		filePath = filepath.Clean(filePath)

		// Ensure the resolved path is within the data directory
		absDataDir, _ := filepath.Abs(cfg.DataDir)
		absFilePath, _ := filepath.Abs(filePath)
		if !strings.HasPrefix(absFilePath, absDataDir+string(os.PathSeparator)) && absFilePath != absDataDir {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// Check if file exists
		info, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "not found", http.StatusNotFound)
			} else {
				logger.Error("failed to stat file", "path", filePath, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}

		// Support Range requests (206 Partial Content)
		w.Header().Set("Accept-Ranges", "bytes")

		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			rangeStr := strings.TrimPrefix(rangeHeader, "bytes=")
			if rangeStr == rangeHeader {
				http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
				return
			}

			parts := strings.SplitN(rangeStr, "-", 2)
			if len(parts) != 2 {
				http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
				return
			}

			fileSize := info.Size()

			var start, end int64
			if parts[0] == "" {
				start = fileSize - mustParseInt64(parts[1])
				end = fileSize - 1
			} else {
				start = mustParseInt64(parts[0])
				if parts[1] != "" {
					end = mustParseInt64(parts[1])
				} else {
					end = fileSize - 1
				}
			}

			if start < 0 || end < 0 || start > end || start >= fileSize {
				w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(fileSize, 10))
				http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
				return
			}

			if end >= fileSize {
				end = fileSize - 1
			}

			contentLength := end - start + 1
			w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(fileSize, 10))
			w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
			w.WriteHeader(http.StatusPartialContent)

			f, err := os.Open(filePath)
			if err != nil {
				logger.Error("failed to open file", "path", filePath, "error", err)
				return
			}
			defer f.Close()

			f.Seek(start, 0)
			buf := make([]byte, contentLength)
			f.Read(buf)
			w.Write(buf)
			return
		}

		// Full file response
		http.ServeFile(w, r, filePath)
	})

	// Health endpoint
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Start server
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	go func() {
		logger.Info("origin server listening", "addr", cfg.ListenAddr, "data_dir", cfg.DataDir)
		var err error
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("received shutdown signal", "signal", sig.String())

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server forced shutdown", "error", err)
	}

	logger.Info("origin server stopped")
}

func mustParseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}


