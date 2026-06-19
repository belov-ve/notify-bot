package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
			// Останавливаем лонг-поллинг Telegram для удаленного инстанса.
			StopTelegramPolling(name)
			// Сбрасываем кэш клиента Matrix для удаленного инстанса.
			if entry.config.Matrix != nil && entry.config.Matrix.Enabled {
				ResetMatrixClient(getAccountID(entry.config.Matrix.Username, entry.config.Matrix.Homeserver))
			}
		} else if entry.port != target.Port {
			slog.Info("Port changed: restarting server", "name", name, "old_port", entry.port, "new_port", target.Port)
			sm.stopServer(name, entry.srv)
			delete(sm.servers, name)
			// Останавливаем лонг-поллинг Telegram для перезапускаемого инстанса.
			StopTelegramPolling(name)
			// Сбрасываем кэш клиента Matrix для перезапускаемого инстанса.
			if entry.config.Matrix != nil && entry.config.Matrix.Enabled {
				ResetMatrixClient(getAccountID(entry.config.Matrix.Username, entry.config.Matrix.Homeserver))
			}
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
			if !isInstanceEqual(entry.config, inst) {
				slog.Info("Configuration updated for instance", "name", name, "port", inst.Port)

				// Если изменились настройки Matrix (включая encryption), сбрасываем кэш старого клиента.
				if entry.config.Matrix != nil && entry.config.Matrix.Enabled {
					ResetMatrixClient(getAccountID(entry.config.Matrix.Username, entry.config.Matrix.Homeserver))
				}

				// Останавливаем лонг-поллинг Telegram, чтобы он перезапустился с новой конфигурацией.
				StopTelegramPolling(name)

				// Обновляем сохраненную конфигурацию в менеджере
				entry.config = inst
				sm.servers[name] = entry
			} else {
				slog.Debug("No changes detected for instance", "name", name)
			}
		}
	}

	// Принудительно запускаем клиентов Matrix с активным прослушиванием (sync: true) на старте или перезагрузке
	InitializeSyncClients(cfg)

	// Принудительно запускаем клиентов Telegram с активным прослушиванием (long polling) на старте или перезагрузке
	InitializeTelegramSyncClients(cfg)
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

// isInstanceEqual выполняет глубокое ручное сравнение двух конфигураций инстансов.
// Это исключает ложные срабатывания, связанные со сравнением указателей в reflect.DeepEqual.
func isInstanceEqual(a, b Instance) bool {
	if a.Name != b.Name || a.Port != b.Port || a.Enabled != b.Enabled ||
		a.TTL != b.TTL || a.BlockDelivery != b.BlockDelivery || a.ShowTime != b.ShowTime {
		return false
	}

	// Сравнение срезов разрешенных IP
	if len(a.AllowedIPs) != len(b.AllowedIPs) {
		return false
	}
	for i := range a.AllowedIPs {
		if a.AllowedIPs[i] != b.AllowedIPs[i] {
			return false
		}
	}

	// Сравнение Telegram конфигураций
	if (a.Telegram == nil) != (b.Telegram == nil) {
		return false
	}
	if a.Telegram != nil {
		at := a.Telegram
		bt := b.Telegram
		if at.Enabled != bt.Enabled || at.BotToken != bt.BotToken || at.ChatID != bt.ChatID ||
			at.RetryCount != bt.RetryCount || at.RetryDelay != bt.RetryDelay || at.Menu != bt.Menu {
			return false
		}
	}

	// Сравнение Matrix конфигураций
	if (a.Matrix == nil) != (b.Matrix == nil) {
		return false
	}
	if a.Matrix != nil {
		am := a.Matrix
		bm := b.Matrix
		if am.Enabled != bm.Enabled || am.Homeserver != bm.Homeserver || am.RoomID != bm.RoomID ||
			am.AccessToken != bm.AccessToken || am.Username != bm.Username || am.Password != bm.Password ||
			am.RetryCount != bm.RetryCount || am.RetryDelay != bm.RetryDelay || am.Encryption != bm.Encryption ||
			am.RecoveryKey != bm.RecoveryKey || am.Menu != bm.Menu {
			return false
		}
	}

	return true
}
