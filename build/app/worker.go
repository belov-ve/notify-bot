package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var (
	wakeUpMu    sync.RWMutex
	wakeUpChans = make(map[string]chan struct{})
)

// getWakeUpChan возвращает или инициализирует канал пробуждения для конкретного канала отправки (instance/service).
func getWakeUpChan(instanceName, service string) chan struct{} {
	key := instanceName + "/" + service
	wakeUpMu.RLock()
	ch, exists := wakeUpChans[key]
	wakeUpMu.RUnlock()
	if exists {
		return ch
	}

	wakeUpMu.Lock()
	defer wakeUpMu.Unlock()
	ch, exists = wakeUpChans[key]
	if !exists {
		ch = make(chan struct{}, 1)
		wakeUpChans[key] = ch
	}
	return ch
}

// WakeUpWorker будит индивидуальный воркер для конкретного инстанса и сервиса.
func WakeUpWorker(instanceName, service string) {
	ch := getWakeUpChan(instanceName, service)
	select {
	case ch <- struct{}{}:
	default:
	}
}

// StartChannelWorker запускает фоновую горутину доставки для конкретного инстанса и сервиса.
func StartChannelWorker(ctx context.Context, db *DBWrapper, cfg *Config, instanceName, service string, interval time.Duration, mediaDir string) {
	slog.Info("Starting channel worker", "instance", instanceName, "service", service, "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Первичное сканирование очереди конкретного канала при старте горутины
	processChannelQueue(db, cfg, instanceName, service)

	wakeCh := getWakeUpChan(instanceName, service)

	for {
		select {
		case <-ctx.Done():
			slog.Info("Channel worker stopped", "instance", instanceName, "service", service)
			return
		case <-wakeCh:
			slog.Debug("Channel worker waked up by signal (new message)", "instance", instanceName, "service", service)
			processChannelQueue(db, cfg, instanceName, service)
		case <-ticker.C:
			processChannelQueue(db, cfg, instanceName, service)
		}
	}
}

// StartGlobalCleanup запускает периодическую очистку осиротевших медиафайлов.
func StartGlobalCleanup(ctx context.Context, db *DBWrapper, mediaDir string) {
	slog.Info("Starting global media cleanup worker", "mediaDir", mediaDir)

	// Очистка при старте
	go CleanOrphanedFiles(db, mediaDir)

	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Global media cleanup worker stopped")
			return
		case <-ticker.C:
			slog.Debug("Running periodic cleanup of orphaned media files...")
			go CleanOrphanedFiles(db, mediaDir)
		}
	}
}

// processChannelQueue извлекает сообщения для конкретного канала и отправляет их.
func processChannelQueue(db *DBWrapper, cfg *Config, instanceName, service string) {
	msgs, err := db.GetPendingMessagesForChannel(instanceName, service)
	if err != nil {
		slog.Error("Failed to fetch pending messages from DB for channel", "instance", instanceName, "service", service, "error", err)
		return
	}

	if len(msgs) == 0 {
		return
	}

	slog.Debug("Processing channel queue", "instance", instanceName, "service", service, "count", len(msgs))

	for _, msg := range msgs {
		configMu.RLock()
		instPtr := cfg.GetInstanceByName(msg.InstanceName)
		var inst *Instance
		if instPtr != nil {
			// Создаем глубокую копию Instance для потокобезопасности воркера
			instCopy := *instPtr
			if instPtr.Telegram != nil {
				tg := *instPtr.Telegram
				instCopy.Telegram = &tg
			}
			if instPtr.Matrix != nil {
				mtx := *instPtr.Matrix
				instCopy.Matrix = &mtx
			}
			inst = &instCopy
		}
		configMu.RUnlock()

		if inst == nil {
			slog.Warn("Instance configuration not found, skipping message",
				"instance", msg.InstanceName, "msgID", msg.ID)
			continue
		}

		isExpired := time.Now().After(msg.TTLDeadline)
		// Сообщение является отложенным, если оно отправляется не с первой попытки (attempts > 0 или status == "failed")
		isDelayed := msg.Attempts > 0 || msg.Status == "failed"
		payload := msg.Payload

		if isDelayed {
			// ВЕТКА 1: Отложенная доставка. ВСЕГДА добавляем маркер [Отложенная доставка] и время создания (не зависит от show_time).
			// Первичное время show_time НЕ добавляется, дублирование меток времени полностью исключено.
			timestampStr := msg.CreatedAt.Local().Format("2006-01-02 15:04:05 MST")
			prefix := "[Отложенная доставка] "
			payload = fmt.Sprintf("%s\n\n%s%s", payload, prefix, timestampStr)
		} else if inst.ShowTime {
			// ВЕТКА 2: Обычная первичная доставка (attempts == 0) при show_time: true.
			// Добавляется ровно одна метка времени приема/создания сообщения ботом.
			timestampStr := msg.CreatedAt.Local().Format("2006-01-02 15:04:05 MST")
			payload = fmt.Sprintf("%s\n\n%s", payload, timestampStr)
		}

		slog.Debug("Processing message from queue", "id", msg.ID, "instance", msg.InstanceName, "service", msg.Service, "attempt", msg.Attempts+1)

		err := attemptSend(inst, &msg, payload, isDelayed)

		if err == nil {
			slog.Info("Message delivered successfully",
				"id", msg.ID, "instance", msg.InstanceName, "service", msg.Service, "delayed", isDelayed)
			// Delete file from disk if no other messages reference it
			cleanUpMessageFile(db, &msg)
			db.DeleteMessage(msg.ID)

			// При успешной отправке сбрасываем задержки для остальных сообщений данного чата и будим воркер
			if resetErr := db.ResetNextAttemptForChannel(msg.InstanceName, msg.Service); resetErr == nil {
				WakeUpWorker(msg.InstanceName, msg.Service)
			}
		} else { // failed
			if isExpired {
				slog.Warn("Message expired after last attempt, removing from queue",
					"id", msg.ID, "deadline", msg.TTLDeadline, "error", err)
				// Delete file from disk when TTL expires
				cleanUpMessageFile(db, &msg)
				db.DeleteMessage(msg.ID)
			} else {
				newAttempts := msg.Attempts + 1
				slog.Warn("Delivery failed, message kept in queue for retry",
					"id", msg.ID, "attempts", newAttempts, "instance", msg.InstanceName, "error", err)

				// Вычисляем задержку ретрая с экспоненциальным backoff.
				delaySec := 2
				if msg.Service == "telegram" && inst.Telegram != nil {
					delaySec = inst.Telegram.RetryDelay
				} else if msg.Service == "matrix" && inst.Matrix != nil {
					delaySec = inst.Matrix.RetryDelay
				}
				if delaySec <= 0 {
					delaySec = 2
				}

				// Экспоненциальный множитель: 2 ^ (attempts - 1)
				backoffFactor := 1 << uint(newAttempts-1)
				if newAttempts > 15 {
					backoffFactor = 1 << 15 // Ограничиваем сдвиг для защиты от переполнения
				}
				actualDelay := time.Duration(delaySec*backoffFactor) * time.Second

				// Ограничиваем паузу ретрая значением MAX_RETRY_DELAY (по умолчанию 300 сек / 5 минут)
				maxRetryDelay := 300 * time.Second
				if envMax := os.Getenv("MAX_RETRY_DELAY"); envMax != "" {
					if sec, parseErr := strconv.Atoi(envMax); parseErr == nil && sec > 0 {
						maxRetryDelay = time.Duration(sec) * time.Second
					}
				}

				if actualDelay > maxRetryDelay {
					actualDelay = maxRetryDelay
				}

				nextAttemptAt := time.Now().Add(actualDelay).Unix()
				db.UpdateMessageStatus(msg.ID, "failed", newAttempts, nextAttemptAt)

				// При сетевой ошибке прерываем цикл выгонки очереди для данного канала,
				// чтобы воркер не блокировался суммой таймаутов на остатке сообщений.
				break
			}
		}
	}
}

// cleanUpMessageFile удаляет временный файл с диска, если на него нет
// ссылок в БД (например, при отправке в несколько каналов одновременно).
func cleanUpMessageFile(db *DBWrapper, msg *Message) {
	if msg.FilePath == "" {
		return
	}
	referenced, err := db.IsFileReferenced(msg.FilePath, msg.ID)
	if err != nil {
		slog.Error("Failed to check file references in DB", "filePath", msg.FilePath, "error", err)
		return
	}
	if !referenced {
		slog.Info("Removing file from disk as it is no longer referenced", "filePath", msg.FilePath)
		if err := os.Remove(msg.FilePath); err != nil && !os.IsNotExist(err) {
			slog.Error("Failed to remove file from disk", "filePath", msg.FilePath, "error", err)
		}
	} else {
		slog.Debug("File is still referenced by other messages, keeping it on disk", "filePath", msg.FilePath)
	}
}

// attemptSend выполняет отправку. Возвращает ошибку в случае неудачи.
func attemptSend(inst *Instance, msg *Message, payload string, isDelayed bool) error {
	// Проверка блокировки отправки (новое имя BlockDelivery).
	if inst.BlockDelivery {
		slog.Warn("DELIVERY BLOCKED: block_delivery is enabled for instance",
			"instance", inst.Name, "id", msg.ID)
		return fmt.Errorf("delivery blocked by block_delivery configuration")
	}

	logMsg := "Sending attempt"
	if isDelayed {
		logMsg = "DELAYED DELIVERY attempt"
	}
	slog.Debug(logMsg, "service", msg.Service, "instance", inst.Name, "id", msg.ID, "attempt", msg.Attempts+1)

	var err error
	switch msg.Service {
	case "telegram":
		if inst.Telegram == nil || !inst.Telegram.Enabled {
			return fmt.Errorf("Telegram is disabled or not configured")
		}
		// Pass file path, original name, and MIME type to send functions
		err = sendTelegramMessage(inst.Telegram.BotToken, inst.Telegram.ChatID, payload,
			inst.Telegram.RetryCount, inst.Telegram.RetryDelay, msg.FilePath, msg.FileName, msg.MimeType)
	case "matrix":
		if inst.Matrix == nil || !inst.Matrix.Enabled {
			return fmt.Errorf("Matrix is disabled or not configured")
		}
		// Pass file path, original name, and MIME type to send functions
		err = sendMatrixWithRetry(inst, payload, msg.FilePath, msg.FileName, msg.MimeType)
	default:
		return fmt.Errorf("unknown service: %s", msg.Service)
	}

	if err != nil {
		slog.Error("Network send error", "service", msg.Service, "error", err, "id", msg.ID)
		return err
	}
	return nil
}

// CleanOrphanedFiles сканирует папку media и удаляет файлы, на которые нет активных
// ссылок в базе данных, если эти файлы были изменены более 10 минут назад (защитный интервал).
func CleanOrphanedFiles(db *DBWrapper, mediaDir string) {
	slog.Debug("Starting cleanup of orphaned media files...", "dir", mediaDir)
	files, err := os.ReadDir(mediaDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("Failed to read media directory for cleanup", "dir", mediaDir, "error", err)
		}
		return
	}

	now := time.Now()
	cleanedCount := 0

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}

		// Защитный интервал: не удаляем файлы, созданные менее 10 минут назад,
		// так как они могут находиться в процессе загрузки или сохранения в БД.
		if now.Sub(info.ModTime()) < 10*time.Minute {
			continue
		}

		filePath := filepath.Join(mediaDir, f.Name())
		
		// Проверяем, ссылается ли хотя бы одно сообщение в БД на этот файл
		var count int
		err = db.db.QueryRow("SELECT COUNT(*) FROM outbox WHERE file_path = ?", filePath).Scan(&count)
		if err != nil {
			slog.Error("Failed to check file references in DB during cleanup", "file", filePath, "error", err)
			continue
		}

		// Если ссылок в БД нет, то файл является сиротой (orphaned) и подлежит удалению
		if count == 0 {
			slog.Info("Removing orphaned file", "path", filePath)
			if err := os.Remove(filePath); err != nil {
				slog.Error("Failed to remove orphaned file", "path", filePath, "error", err)
			} else {
				cleanedCount++
			}
		}
	}

	if cleanedCount > 0 {
		slog.Info("Orphaned media files cleanup completed", "count", cleanedCount)
	} else {
		slog.Debug("No orphaned media files found")
	}
}
