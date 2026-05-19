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
	"strings"
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
	envLevel := strings.ToUpper(os.Getenv("LOG_LEVEL"))

	switch envLevel {
	case "DEBUG":
		logLevel = slog.LevelDebug
	case "WARN", "WARNING":
		logLevel = slog.LevelWarn
	case "ERROR":
		logLevel = slog.LevelError
	case "INFO":
		logLevel = slog.LevelInfo
	default:
		// Если переменная не задана или содержит неизвестное значение, оставляем INFO.
		// В docker-compose обычно задано `${LOG_LEVEL:-DEBUG}`, так что сюда попадем только при пустом вводе.
		if envLevel == "" {
			logLevel = slog.LevelInfo
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))
	slog.Info("Logger initialized", "level", logLevel.String())
	slog.Debug("Debug logging is active")

	slog.Info("Starting notify-bot v2.2.3 (Matrix E2EE & Formatting)")

	// 2. Инициализация Базы Данных SQLite.
	dbPath := "/app/data/notify_bot.db"
	if envPath := os.Getenv("DB_PATH"); envPath != "" {
		dbPath = envPath
	}
	slog.Debug("Initializing database", "path", dbPath)
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
	slog.Debug("Loading configuration", "path", configPath)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		logAndExit("Failed to load initial config", "error", err)
	}
	globalConfig = cfg
	slog.Debug("Configuration loaded successfully", "instances_count", len(cfg.Instances))

	// 4. Запуск глобального сервера мониторинга (Health + Stats).
	var monitorServer *http.Server
	if healthPortStr := os.Getenv("HEALTH_CHECK_PORT"); healthPortStr != "" {
		healthPort, _ := strconv.Atoi(healthPortStr)
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			slog.Debug("Monitor health check request", "remote", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "2.2.3"})
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
