package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// logAndExit логирует фатальную ошибку и завершает программу.
func logAndExit(msg string, args ...interface{}) {
	slog.Error(msg, args...)
	time.Sleep(100 * time.Millisecond)
	os.Exit(1)
}

func main() {
	// 1. Настройка логирования из переменных окружения.
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		logLevel = slog.LevelDebug
	} else if os.Getenv("LOG_LEVEL") == "WARNING" {
		logLevel = slog.LevelWarn
	} else if os.Getenv("LOG_LEVEL") == "ERROR" {
		logLevel = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))

	slog.Info("Starting notify-bot v2.1.0 (Guaranteed Delivery)")

	// 2. Инициализация Базы Данных SQLite.
	dbPath := "/app/notify_queue.db"
	if envPath := os.Getenv("DB_PATH"); envPath != "" {
		dbPath = envPath
	}
	db, err := InitDB(dbPath)
	if err != nil {
		logAndExit("Failed to initialize database", "error", err)
	}
	globalDB = db
	defer db.Close()

	// 3. Загрузка конфигурации.
	configPath := "/app/config.yml"
	if envCfg := os.Getenv("CONFIG_PATH"); envCfg != "" {
		configPath = envCfg
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		logAndExit("Failed to load initial config", "error", err)
	}
	globalConfig = cfg // Сохраняем для обработчика статистики

	// 4. Запуск глобального сервера мониторинга (Health + Stats).
	var monitorServer *http.Server
	if healthPortStr := os.Getenv("HEALTH_CHECK_PORT"); healthPortStr != "" {
		healthPort, _ := strconv.Atoi(healthPortStr)
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			slog.Debug("Monitor health check request", "remote", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "2.1.0"})
		})
		mux.HandleFunc("/stats", statsHandler) // Глобальная статистика очередей.
		
		monitorServer = &http.Server{
			Addr:    fmt.Sprintf(":%d", healthPort),
			Handler: mux,
		}
		go func() {
			slog.Info("Starting monitor server (health/stats)", "port", healthPort)
			if err := monitorServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Monitor server error", "error", err)
			}
		}()
	}

	// 5. Запуск Воркера (обработка очереди).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	retryInterval := 60 * time.Second
	if envInt := os.Getenv("RETRY_INTERVAL"); envInt != "" {
		if val, err := strconv.Atoi(envInt); err == nil && val > 0 {
			retryInterval = time.Duration(val) * time.Second
		}
	}
	// Воркер работает с текущим конфигом.
	go StartWorker(ctx, db, cfg, retryInterval)

	// 6. Запуск и управление серверами инстансов.
	manager := NewServerManager()
	manager.UpdateServers(cfg)

	// 7. Механизм динамического релоада конфигурации.
	go func() {
		lastMod := time.Now()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				info, err := os.Stat(configPath)
				if err == nil && info.ModTime().After(lastMod) {
					slog.Info("Config file changed, reloading...", "file", configPath)
					newCfg, err := LoadConfig(configPath)
					if err == nil {
						// Обновляем серверы и конфиг для воркера и статистики.
						manager.UpdateServers(newCfg)
						globalConfig = newCfg
						*cfg = *newCfg 
						lastMod = info.ModTime()
						slog.Info("Configuration reloaded successfully")
					} else {
						slog.Error("Failed to reload config", "error", err)
					}
				}
			}
		}
	}()

	// 8. Ожидание сигнала завершения (Graceful Shutdown).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("Shutting down bot...")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()

	if monitorServer != nil {
		monitorServer.Shutdown(stopCtx)
	}
	manager.StopAll(stopCtx)
	cancel() // Остановка воркера.

	slog.Info("Notify-bot stopped.")
}