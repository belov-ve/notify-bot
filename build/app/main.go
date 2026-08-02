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
	if envLevel != "" && envLevel != "DEBUG" && envLevel != "WARN" && envLevel != "WARNING" && envLevel != "ERROR" && envLevel != "INFO" {
		slog.Warn("Unknown LOG_LEVEL, falling back to INFO", "got", envLevel)
	}
	slog.Info("Logger initialized", "level", logLevel.String())
	slog.Debug("Debug logging is active")

	// Выводим информационное сообщение о запуске приложения с указанием версии.
	slog.Info("Starting notify-bot v3.5.1")

	// 2. Инициализация Базы Данных SQLite.
	dbPath := "/app/data/notify_bot.db"
	if envPath := os.Getenv("DB_PATH"); envPath != "" {
		dbPath = envPath
	}

	// Создаем директорию для временных медиафайлов/вложений рядом с БД.
	mediaPath := getMediaDir()
	if err := os.MkdirAll(mediaPath, 0755); err != nil {
		slog.Error("Failed to create media directory", "path", mediaPath, "error", err)
	} else {
		slog.Debug("Media directory checked/created", "path", mediaPath)
	}

	slog.Debug("Initializing database", "path", dbPath)
	db, err := InitDB(dbPath)
	if err != nil {
		logAndExit("Failed to initialize database", "error", err)
	}
	globalDB = db
	defer db.Close()

	// Переводим все неотправленные сообщения в статус failed при старте бота,
	// чтобы при последующих попытках отправки они шли с пометкой отложенной доставки.
	if err := db.MarkPendingAsFailed(); err != nil {
		logAndExit("Failed to mark pending messages as failed", "error", err)
	}

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
			// Возвращаем статус успешной проверки здоровья и текущую версию приложения.
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "3.5.1"})
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

	// 5. Запуск глобальной очистки медиафайлов.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	retryInterval := 60 * time.Second
	if envInt := os.Getenv("RETRY_INTERVAL"); envInt != "" {
		if val, err := strconv.Atoi(envInt); err == nil && val > 0 {
			retryInterval = time.Duration(val) * time.Second
		}
	}
	go StartGlobalCleanup(ctx, db, mediaPath)

	// 6. Запуск и управление серверами и воркерами инстансов.
	manager := NewServerManager(retryInterval, mediaPath)
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
						
						// Блокируем для безопасного обновления глобального конфига
						configMu.Lock()
						globalConfig = newCfg
						*cfg = *newCfg
						configMu.Unlock()
						
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
	// Останавливаем все активные сессии лонг-поллинга Telegram при завершении работы.
	StopAllTelegramPolling()
	cancel() // Остановка воркера.

	slog.Info("Notify-bot stopped.")
}
