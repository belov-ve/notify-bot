package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"
)

// ServerManager управляет жизненным циклом HTTP-серверов для каждого инстанса.
type ServerManager struct {
	servers map[string]serverEntry // name -> entry
	mu      sync.Mutex
}

type serverEntry struct {
	srv    *http.Server
	port   int
	config Instance // Сохраняем полную конфигурацию для сравнения
}

// NewServerManager создает новый экземпляр менеджера серверов.
func NewServerManager() *ServerManager {
	return &ServerManager{
		servers: make(map[string]serverEntry),
	}
}

// UpdateServers синхронизирует работающие серверы с новой конфигурацией.
func (sm *ServerManager) UpdateServers(cfg *Config) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	slog.Info("Syncing servers with new configuration...")

	// 1. Составляем список целевых активных инстансов.
	targetInstances := make(map[string]Instance)
	for _, inst := range cfg.Instances {
		if !inst.Enabled {
			continue
		}
		hasTelegram := inst.Telegram != nil && inst.Telegram.Enabled
		hasMatrix := inst.Matrix != nil && inst.Matrix.Enabled

		if hasTelegram || hasMatrix {
			targetInstances[inst.Name] = inst
		}
	}

	// 2. Останавливаем серверы, которые удалены или у которых изменился порт.
	for name, entry := range sm.servers {
		target, exists := targetInstances[name]
		if !exists {
			slog.Info("Removing instance: stopping server", "name", name, "port", entry.port)
			sm.stopServer(name, entry.srv)
			delete(sm.servers, name)
		} else if entry.port != target.Port {
			slog.Info("Port changed: restarting server", "name", name, "old_port", entry.port, "new_port", target.Port)
			sm.stopServer(name, entry.srv)
			delete(sm.servers, name)
		}
	}

	// 3. Запускаем новые серверы или логируем реальные изменения настроек.
	for name, inst := range targetInstances {
		entry, running := sm.servers[name]
		if !running {
			slog.Info("Starting new instance server", "name", name, "port", inst.Port)
			slog.Debug("Instance config details",
				"name", name,
				"allowed_ips", inst.AllowedIPs,
				"telegram_enabled", inst.Telegram != nil && inst.Telegram.Enabled,
				"matrix_enabled", inst.Matrix != nil && inst.Matrix.Enabled,
				"block_delivery", inst.BlockDelivery)

			mux := http.NewServeMux()
			instanceName := name
			mux.HandleFunc("/notify", func(w http.ResponseWriter, r *http.Request) {
				notifyHandler(w, r, instanceName)
			})

			srv := &http.Server{
				Addr:         fmt.Sprintf(":%d", inst.Port),
				Handler:      mux,
				ReadTimeout:  5 * time.Second,
				WriteTimeout: 10 * time.Second,
			}

			sm.servers[name] = serverEntry{srv: srv, port: inst.Port, config: inst}
			go func(s *http.Server, n string) {
				if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					slog.Error("Server failed", "instance", n, "error", err)
				}
			}(srv, name)
		} else {
			// Сервер уже запущен на правильном порту.
			// Проверяем, изменилось ли что-то внутри конфигурации инстанса.
			if !reflect.DeepEqual(entry.config, inst) {
				slog.Info("Configuration updated for instance", "name", name, "port", inst.Port)

				// Если изменились настройки Matrix (включая encryption), сбрасываем кэш клиента.
				ResetMatrixClient(name)

				// Обновляем сохраненную конфигурацию в менеджере
				entry.config = inst
				sm.servers[name] = entry
			} else {
				slog.Debug("No changes detected for instance", "name", name)
			}
		}
	}
}

// stopServer выполняет корректную остановку сервера.
func (sm *ServerManager) stopServer(name string, srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("Server shutdown error", "name", name, "error", err)
	}
}

// StopAll корректно останавливает все запущенные серверы.
func (sm *ServerManager) StopAll(ctx context.Context) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	var wg sync.WaitGroup
	for name, entry := range sm.servers {
		wg.Add(1)
		go func(n string, s *http.Server) {
			defer wg.Done()
			slog.Info("Shutting down server", "instance", n)
			s.Shutdown(ctx)
		}(name, entry.srv)
	}
	wg.Wait()
}
