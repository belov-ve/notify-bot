package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// ServerManager управляет жизненным циклом HTTP-серверов и воркеров отправки для каждого инстанса.
type ServerManager struct {
	servers       map[string]serverEntry        // name -> entry
	workerCancels map[string]context.CancelFunc // key (instance/service) -> cancelFunc
	retryInterval time.Duration
	mediaPath     string
	mu            sync.Mutex

	// Планировщик задач по расписанию (cron)
	cron        *cron.Cron
	cronEntries map[string][]cron.EntryID // instanceName -> EntryIDs
	lastConfig  *Config                   // Сохраненная конфигурация для сравнения при hot-reload
}

type serverEntry struct {
	srv    *http.Server
	port   int
	config Instance // Сохраняем полную конфигурацию для сравнения
}

// NewServerManager создает новый экземпляр менеджера серверов.
func NewServerManager(retryInterval time.Duration, mediaPath string) *ServerManager {
	sm := &ServerManager{
		servers:       make(map[string]serverEntry),
		workerCancels: make(map[string]context.CancelFunc),
		retryInterval: retryInterval,
		mediaPath:     mediaPath,
		cron:          cron.New(),
		cronEntries:   make(map[string][]cron.EntryID),
	}
	// Запускаем глобальный планировщик при создании менеджера
	sm.cron.Start()
	return sm
}

// UpdateServers синхронизирует работающие серверы и воркеры доставки с новой конфигурацией.
func (sm *ServerManager) UpdateServers(cfg *Config) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	slog.Info("Syncing servers and delivery workers with new configuration...")

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

	// 2. Останавливаем серверы и воркеры, которые удалены или у которых изменился порт.
	for name, entry := range sm.servers {
		target, exists := targetInstances[name]
		if !exists {
			slog.Info("Removing instance: stopping server and workers", "name", name, "port", entry.port)
			sm.stopServer(name, entry.srv)
			delete(sm.servers, name)
			// Останавливаем воркеры доставки
			sm.stopWorkerForChannel(name, "telegram")
			sm.stopWorkerForChannel(name, "matrix")
			// Останавливаем лонг-поллинг Telegram для удаленного инстанса.
			StopTelegramPolling(name)
			// Останавливаем cron-задачи для удаленного инстанса.
			sm.stopCronTasks(name)
			// Сбрасываем кэш клиента Matrix для удаленного инстанса.
			if entry.config.Matrix != nil && entry.config.Matrix.Enabled {
				ResetMatrixClient(getAccountID(entry.config.Matrix.Username, entry.config.Matrix.Homeserver))
			}
		} else if entry.port != target.Port {
			slog.Info("Port changed: restarting server and workers", "name", name, "old_port", entry.port, "new_port", target.Port)
			sm.stopServer(name, entry.srv)
			delete(sm.servers, name)
			// Останавливаем воркеры доставки перед перезапуском
			sm.stopWorkerForChannel(name, "telegram")
			sm.stopWorkerForChannel(name, "matrix")
			// Останавливаем лонг-поллинг Telegram для перезапускаемого инстанса.
			StopTelegramPolling(name)
			// Останавливаем cron-задачи перезапускаемого инстанса.
			sm.stopCronTasks(name)
			// Сбрасываем кэш клиента Matrix для перезапускаемого инстанса.
			if entry.config.Matrix != nil && entry.config.Matrix.Enabled {
				ResetMatrixClient(getAccountID(entry.config.Matrix.Username, entry.config.Matrix.Homeserver))
			}
		}
	}

	// 3. Запускаем новые серверы/воркеры или логируем реальные изменения настроек.
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

			// Запускаем воркеры доставки для нового инстанса
			if inst.Telegram != nil && inst.Telegram.Enabled {
				sm.startWorkerForChannel(cfg, name, "telegram")
			}
			if inst.Matrix != nil && inst.Matrix.Enabled {
				sm.startWorkerForChannel(cfg, name, "matrix")
			}
			// Запускаем cron-задачи для нового инстанса
			sm.startCronTasks(cfg, name, &inst)
		} else {
			// Сервер уже запущен на правильном порту.
			// Проверяем, изменилось ли что-то внутри конфигурации инстанса или списка cron-задач.
			var oldTaskList *TaskList
			if sm.lastConfig != nil {
				oldTaskList = findTaskListByID(sm.lastConfig, entry.config.Tasks)
			}
			newTaskList := findTaskListByID(cfg, inst.Tasks)

			if !isInstanceEqual(entry.config, inst) || !areTaskListsEqual(oldTaskList, newTaskList) {
				slog.Info("Configuration updated for instance", "name", name, "port", inst.Port)

				// Перезапускаем воркеры с новой конфигурацией
				sm.stopWorkerForChannel(name, "telegram")
				sm.stopWorkerForChannel(name, "matrix")
				if inst.Telegram != nil && inst.Telegram.Enabled {
					sm.startWorkerForChannel(cfg, name, "telegram")
				}
				if inst.Matrix != nil && inst.Matrix.Enabled {
					sm.startWorkerForChannel(cfg, name, "matrix")
				}

				// Если изменились настройки Matrix (включая encryption), сбрасываем кэш старого клиента.
				if entry.config.Matrix != nil && entry.config.Matrix.Enabled {
					ResetMatrixClient(getAccountID(entry.config.Matrix.Username, entry.config.Matrix.Homeserver))
				}

				// Останавливаем лонг-поллинг Telegram, чтобы он перезапустился с новой конфигурацией.
				StopTelegramPolling(name)

				// Перезапускаем cron-задачи для обновленного инстанса
				sm.startCronTasks(cfg, name, &inst)

				// Обновляем сохраненную конфигурацию в менеджере
				entry.config = inst
				sm.servers[name] = entry
			} else {
				slog.Debug("No changes detected for instance", "name", name)
				// На всякий случай гарантируем, что воркеры работают (например, если они были случайно остановлены)
				if inst.Telegram != nil && inst.Telegram.Enabled {
					sm.startWorkerForChannel(cfg, name, "telegram")
				}
				if inst.Matrix != nil && inst.Matrix.Enabled {
					sm.startWorkerForChannel(cfg, name, "matrix")
				}
				// Cron-задачи не пересоздаются, если конфигурация инстанса не изменилась, 
				// чтобы сохранить непрерывность выполнения текущего расписания.
			}
		}
	}

	// Принудительно запускаем клиентов Matrix с активным прослушиванием (sync: true) на старте или перезагрузке
	InitializeSyncClients(cfg)

	// Принудительно запускаем клиентов Telegram с активным прослушиванием (long polling) на старте или перезагрузке
	InitializeTelegramSyncClients(cfg)

	// Сохраняем текущую конфигурацию для последующего сравнения
	sm.lastConfig = cfg
}

// startWorkerForChannel запускает воркер канала, если он еще не запущен.
func (sm *ServerManager) startWorkerForChannel(cfg *Config, instanceName, service string) {
	key := instanceName + "/" + service
	if _, running := sm.workerCancels[key]; running {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	sm.workerCancels[key] = cancel

	go StartChannelWorker(ctx, globalDB, cfg, instanceName, service, sm.retryInterval, sm.mediaPath)
}

// stopWorkerForChannel останавливает воркер канала, если он запущен.
func (sm *ServerManager) stopWorkerForChannel(instanceName, service string) {
	key := instanceName + "/" + service
	if cancel, running := sm.workerCancels[key]; running {
		cancel()
		delete(sm.workerCancels, key)
	}
}

// stopCronTasks останавливает все запланированные задачи для инстанса.
func (sm *ServerManager) stopCronTasks(instanceName string) {
	if entries, ok := sm.cronEntries[instanceName]; ok {
		for _, entryID := range entries {
			sm.cron.Remove(entryID)
		}
		delete(sm.cronEntries, instanceName)
		slog.Info("Stopped cron tasks for instance", "instance", instanceName)
	}
}

// startCronTasks планирует и запускает задачи из подключенного списка tasks для инстанса.
func (sm *ServerManager) startCronTasks(cfg *Config, instanceName string, inst *Instance) {
	// Сначала останавливаем существующие задачи инстанса, чтобы избежать дублирования
	sm.stopCronTasks(instanceName)

	if inst.Tasks == "" {
		return
	}

	// Ищем именованный список задач в глобальной конфигурации
	var targetTaskList *TaskList
	for i := range cfg.Tasks {
		if cfg.Tasks[i].ID == inst.Tasks {
			targetTaskList = &cfg.Tasks[i]
			break
		}
	}

	if targetTaskList == nil {
		slog.Warn("Configured Tasks ID not found", "tasksID", inst.Tasks, "instance", instanceName)
		return
	}

	var entries []cron.EntryID
	for _, item := range targetTaskList.Items {
		taskItem := item // Копия для безопасного замыкания

		// Проверяем, активна ли задача. Если enabled равен nil (не задан), то по умолчанию задача активна (true).
		if taskItem.Enabled != nil && !*taskItem.Enabled {
			slog.Info("Scheduled task is disabled, skipping registration", "task", taskItem.Name, "instance", instanceName)
			continue
		}

		// Валидация cron выражения на этапе планирования
		_, err := cron.ParseStandard(taskItem.Schedule)
		if err != nil {
			slog.Error("Invalid cron schedule for task", "task", taskItem.Name, "schedule", taskItem.Schedule, "error", err)
			continue
		}

		// Создаем глубокую копию Instance для безопасного использования внутри горутин cron-задач
		instCopy := *inst
		if inst.Telegram != nil {
			tg := *inst.Telegram
			instCopy.Telegram = &tg
		}
		if inst.Matrix != nil {
			mtx := *inst.Matrix
			instCopy.Matrix = &mtx
		}

		entryID, err := sm.cron.AddFunc(taskItem.Schedule, func() {
			sm.executeCronTask(&instCopy, taskItem)
		})

		if err != nil {
			slog.Error("Failed to schedule task", "task", taskItem.Name, "error", err)
			continue
		}

		entries = append(entries, entryID)
		slog.Info("Scheduled task", "task", taskItem.Name, "schedule", taskItem.Schedule, "instance", instanceName)
	}

	if len(entries) > 0 {
		sm.cronEntries[instanceName] = entries
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

// StopAll корректно останавливает все запущенные серверы и воркеры доставки.
func (sm *ServerManager) StopAll(ctx context.Context) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Останавливаем глобальный планировщик задач
	slog.Info("Stopping global cron scheduler")
	sm.cron.Stop()

	// Останавливаем все воркеры доставки
	for key, cancel := range sm.workerCancels {
		slog.Info("Stopping channel worker", "channel", key)
		cancel()
		delete(sm.workerCancels, key)
	}

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
	// Сравниваем базовые поля, включая Tasks (список задач планировщика) для корректного hot-reload
	if a.Name != b.Name || a.Port != b.Port || a.Enabled != b.Enabled ||
		a.TTL != b.TTL || a.BlockDelivery != b.BlockDelivery || a.ShowTime != b.ShowTime ||
		a.Tasks != b.Tasks {
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

// findTaskListByID ищет список задач TaskList по его ID в структуре Config.
func findTaskListByID(cfg *Config, id string) *TaskList {
	if cfg == nil || id == "" {
		return nil
	}
	for i := range cfg.Tasks {
		if cfg.Tasks[i].ID == id {
			return &cfg.Tasks[i]
		}
	}
	return nil
}

// areTaskListsEqual сравнивает два списка задач на равенство для детекции изменений.
func areTaskListsEqual(listA, listB *TaskList) bool {
	if (listA == nil) != (listB == nil) {
		return false
	}
	if listA == nil {
		return true
	}
	if listA.ID != listB.ID {
		return false
	}
	if len(listA.Items) != len(listB.Items) {
		return false
	}
	for i := range listA.Items {
		itemA := listA.Items[i]
		itemB := listB.Items[i]
		if itemA.Name != itemB.Name ||
			itemA.Schedule != itemB.Schedule ||
			itemA.URL != itemB.URL ||
			itemA.Script != itemB.Script ||
			itemA.Description != itemB.Description {
			return false
		}
		// Сравнение Enabled флагов (указателей на bool)
		if (itemA.Enabled == nil) != (itemB.Enabled == nil) {
			return false
		}
		if itemA.Enabled != nil {
			if *itemA.Enabled != *itemB.Enabled {
				return false
			}
		}
	}
	return true
}
