package main

import (
    "context"
    "fmt"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "strconv"
    "syscall"
    "time"
)

// logAndExit логирует фатальную ошибку и завершает программу с кодом 1.
// Используется при ошибках конфигурации, запуска серверов и т.д.
// Код 1 заставляет Docker перезапускать контейнер (если политика restart позволяет).
func logAndExit(msg string, args ...interface{}) {
    slog.Error(msg, args...)
    time.Sleep(100 * time.Millisecond) // небольшая пауза для гарантии записи лога
    os.Exit(1)
}

func main() {
    // ----- Настройка уровня логирования из переменной окружения LOG_LEVEL -----
    // Поддерживаются: DEBUG, INFO, WARNING, ERROR. По умолчанию INFO.
    logLevel := slog.LevelInfo
    switch os.Getenv("LOG_LEVEL") {
    case "DEBUG":
        logLevel = slog.LevelDebug
    case "WARNING":
        logLevel = slog.LevelWarn
    case "ERROR":
        logLevel = slog.LevelError
    default:
        logLevel = slog.LevelInfo
    }
    opts := &slog.HandlerOptions{Level: logLevel}
    handler := slog.NewTextHandler(os.Stdout, opts)
    slog.SetDefault(slog.New(handler))

    slog.Info("Starting notify-bot (Go version)")

    // ----- Health check сервер на отдельном порту (опционально) -----
    // Порт задаётся переменной окружения HEALTH_CHECK_PORT (например, 8040).
    // Этот сервер не проверяет IP (доступен всем), логирует только на DEBUG,
    // и не зависит от конфигурации экземпляров. Идеален для Docker healthcheck.
    var healthServer *http.Server
    if healthPortStr := os.Getenv("HEALTH_CHECK_PORT"); healthPortStr != "" {
        healthPort, err := strconv.Atoi(healthPortStr)
        if err != nil {
            logAndExit("Invalid HEALTH_CHECK_PORT", "port", healthPortStr, "error", err)
        }
        healthMux := http.NewServeMux()
        healthMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
            // Логируем только на DEBUG, чтобы не засорять логи при частых проверках
            slog.Debug("Health check request", "remote", r.RemoteAddr)
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusOK)
            w.Write([]byte(`{"status":"ok"}`))
        })
        healthServer = &http.Server{
            Addr:         fmt.Sprintf(":%d", healthPort),
            Handler:      healthMux,
            ReadTimeout:  2 * time.Second,
            WriteTimeout: 2 * time.Second,
            IdleTimeout:  5 * time.Second,
        }
        go func() {
            slog.Info("Starting health check server", "port", healthPort)
            if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                logAndExit("Health check server failed", "error", err)
            }
        }()
    } else {
        slog.Info("HEALTH_CHECK_PORT not set, health check server disabled")
    }

    // ----- Загрузка основной конфигурации (config.yml) -----
    cfg, err := LoadConfig("/app/config.yml")
    if err != nil {
        logAndExit("Failed to load config", "error", err)
    }

    var servers []*http.Server
    errChan := make(chan error, len(cfg.Instances)) // канал для ошибок запуска серверов

    // ----- Создание и запуск HTTP-серверов для каждого активного экземпляра -----
    for _, inst := range cfg.Instances {
        // Пропускаем отключённые экземпляры (enabled: false)
        if !inst.Enabled {
            slog.Info("Skipping disabled instance", "name", inst.Name, "port", inst.Port)
            continue
        }

        // Проверяем, что есть хотя бы один активный канал уведомлений (Telegram или Matrix)
        hasTelegram := inst.Telegram != nil && inst.Telegram.Enabled && inst.Telegram.BotToken != "" && inst.Telegram.ChatID != ""
        hasMatrix := inst.Matrix != nil && inst.Matrix.Enabled && inst.Matrix.Homeserver != "" && inst.Matrix.Username != "" && inst.Matrix.RoomID != "" &&
            (inst.Matrix.AccessToken != "" || inst.Matrix.Password != "")
        if !hasTelegram && !hasMatrix {
            logAndExit("Instance has no active notification channels", "name", inst.Name, "port", inst.Port)
        }

        // Детальное логирование настроек экземпляра (уровень DEBUG)
        for _, cidr := range inst.AllowedIPs {
            slog.Debug("Instance allowed network", "instance", inst.Name, "network", cidr)
        }
        if hasTelegram {
            slog.Debug("Instance Telegram enabled", "instance", inst.Name,
                "retry_count", inst.Telegram.RetryCount,
                "retry_delay", inst.Telegram.RetryDelay)
        }
        if hasMatrix {
            slog.Debug("Instance Matrix enabled", "instance", inst.Name,
                "retry_count", inst.Matrix.RetryCount,
                "retry_delay", inst.Matrix.RetryDelay)
        }

        // Создаём маршрутизатор с эндпоинтами /health и /notify
        mux := http.NewServeMux()
        mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
            healthHandler(w, r, inst.Name)
        })
        mux.HandleFunc("/notify", func(w http.ResponseWriter, r *http.Request) {
            notifyHandler(w, r, &inst)
        })

        // HTTP-сервер с таймаутами для предотвращения зависаний
        srv := &http.Server{
            Addr:         fmt.Sprintf(":%d", inst.Port),
            Handler:      mux,
            ReadTimeout:  5 * time.Second,
            WriteTimeout: 10 * time.Second,
            IdleTimeout:  15 * time.Second,
        }

        // Запускаем сервер в отдельной горутине
        go func(s *http.Server, name string, port int) {
            slog.Info("Starting instance", "name", name, "port", port)
            if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                slog.Error("Server error", "name", name, "error", err)
                errChan <- fmt.Errorf("instance %s on port %d: %w", name, port, err)
            }
        }(srv, inst.Name, inst.Port)
        servers = append(servers, srv)
    }

    // Если нет ни одного работающего экземпляра и health check сервер отключён – выходим
    if len(servers) == 0 && healthServer == nil {
        logAndExit("No valid instances to run and health check server disabled")
    }

    // Мониторинг ошибок запуска основных серверов (например, порт уже занят)
    go func() {
        select {
        case err := <-errChan:
            logAndExit("Fatal: server startup failed", "error", err)
        }
    }()

    // ----- Graceful shutdown: ожидание сигнала SIGINT (Ctrl+C) или SIGTERM (docker stop) -----
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit
    slog.Info("Shutting down servers...")

    // Даём серверам 10 секунд на завершение обработки текущих запросов
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    // Останавливаем все основные HTTP-серверы
    for _, srv := range servers {
        if err := srv.Shutdown(ctx); err != nil {
            slog.Error("Server shutdown error", "error", err)
        }
    }
    // Останавливаем health check сервер, если он был запущен
    if healthServer != nil {
        if err := healthServer.Shutdown(ctx); err != nil {
            slog.Error("Health check server shutdown error", "error", err)
        }
    }

    // Ожидаем завершения всех горутин, отправляющих сообщения (Telegram/Matrix)
    // (WaitGroup определена в handlers.go и увеличивается перед каждой отправкой)
    slog.Info("Waiting for pending notifications to complete...")
    done := make(chan struct{})
    go func() {
        wg.Wait() // блокируется, пока счётчик WaitGroup не станет нулевым
        close(done)
    }()
    select {
    case <-done:
        slog.Info("All notifications sent")
    case <-time.After(30 * time.Second):
        // Таймаут на случай, если какая-то отправка зависла навсегда
        slog.Warn("Timeout waiting for notifications, forcing exit")
    }

    slog.Info("All servers stopped")
}