package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// wakeUpChan – канал для мгновенного пробуждения воркера.
var wakeUpChan = make(chan struct{}, 1)

// StartWorker запускает фоновый процесс обработки очереди.
func StartWorker(ctx context.Context, db *DBWrapper, cfg *Config, interval time.Duration, mediaDir string) {
	slog.Info("Starting delivery worker", "interval", interval, "mediaDir", mediaDir)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Запускаем очистку осиротевших файлов в фоне при старте
	go CleanOrphanedFiles(db, mediaDir)

	// Запускаем тикер для периодической очистки осиротевших файлов (раз в 12 часов)
	cleanupTicker := time.NewTicker(12 * time.Hour)
	defer cleanupTicker.Stop()

	slog.Debug("Performing initial queue scan...")
	processQueue(db, cfg)

	for {
		select {
		case <-ctx.Done():
			slog.Info("Delivery worker received stop signal, exiting loop...")
			return
		case <-wakeUpChan:
			slog.Debug("Worker waked up by signal (new message)")
			processQueue(db, cfg)
		case <-ticker.C:
			processQueue(db, cfg)
		case <-cleanupTicker.C:
			slog.Debug("Running periodic cleanup of orphaned media files...")
			go CleanOrphanedFiles(db, mediaDir)
		}
	}
}

// processQueue извлекает сообщения и отправляет их.
func processQueue(db *DBWrapper, cfg *Config) {
	msgs, err := db.GetPendingMessages()
	if err != nil {
		slog.Error("Failed to fetch pending messages from DB", "error", err)
		return
	}

	if len(msgs) == 0 {
		return
	}

	slog.Debug("Processing queue", "count", len(msgs))

	for _, msg := range msgs {
		inst := cfg.GetInstanceByName(msg.InstanceName)
		if inst == nil {
			slog.Warn("Instance configuration not found, skipping message",
				"instance", msg.InstanceName, "msgID", msg.ID)
			continue
		}

		isExpired := time.Now().After(msg.TTLDeadline)

		// Подготовка Payload. Если это ретрай — добавляем пометку об отложенной доставке.
		payload := msg.Payload
		isDelayed := msg.Attempts > 0
		if isDelayed {
			timestampStr := msg.CreatedAt.Local().Format("2006-01-02 15:04:05 MST")
			prefix := "[Отложенная доставка] "

			// Если в полезной нагрузке уже есть метка времени (ShowTime=true),
			// заменяем её на версию с префиксом.
			if strings.HasSuffix(payload, timestampStr) {
				payload = strings.TrimSuffix(payload, timestampStr) + prefix + timestampStr
			} else {
				// Если метки нет, добавляем её через две пустые строки.
				payload = fmt.Sprintf("%s\n\n%s%s", payload, prefix, timestampStr)
			}
		}

		slog.Debug("Processing message from queue", "id", msg.ID, "instance", msg.InstanceName, "service", msg.Service, "attempt", msg.Attempts+1)

		success := attemptSend(inst, &msg, payload, isDelayed)

		if success {
			slog.Info("Message delivered successfully",
				"id", msg.ID, "instance", msg.InstanceName, "service", msg.Service, "delayed", isDelayed)
			// Delete file from disk if no other messages reference it
			cleanUpMessageFile(db, &msg)
			db.DeleteMessage(msg.ID)
		} else {
			if isExpired {
				slog.Warn("Message expired after last attempt, removing from queue",
					"id", msg.ID, "deadline", msg.TTLDeadline)
				// Delete file from disk when TTL expires
				cleanUpMessageFile(db, &msg)
				db.DeleteMessage(msg.ID)
			} else {
				newAttempts := msg.Attempts + 1
				slog.Warn("Delivery failed, message kept in queue for retry",
					"id", msg.ID, "attempts", newAttempts, "instance", msg.InstanceName)
				db.UpdateMessageStatus(msg.ID, "failed", newAttempts)
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

// attemptSend выполняет отправку. Принимает уже подготовленный payload.
func attemptSend(inst *Instance, msg *Message, payload string, isDelayed bool) bool {
	// Проверка блокировки отправки (новое имя BlockDelivery).
	if inst.BlockDelivery {
		slog.Warn("DELIVERY BLOCKED: block_delivery is enabled for instance",
			"instance", inst.Name, "id", msg.ID)
		return false
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
			slog.Error("Telegram is disabled or not configured", "instance", inst.Name)
			return false
		}
		// Pass file path, original name, and MIME type to send functions
		err = sendTelegramMessage(inst.Telegram.BotToken, inst.Telegram.ChatID, payload,
			inst.Telegram.RetryCount, inst.Telegram.RetryDelay, msg.FilePath, msg.FileName, msg.MimeType)
	case "matrix":
		if inst.Matrix == nil || !inst.Matrix.Enabled {
			slog.Error("Matrix is disabled or not configured", "instance", inst.Name)
			return false
		}
		// Pass file path, original name, and MIME type to send functions
		err = sendMatrixWithRetry(inst, payload, msg.FilePath, msg.FileName, msg.MimeType)
	default:
		return false
	}

	if err != nil {
		slog.Error("Network send error", "service", msg.Service, "error", err, "id", msg.ID)
		return false
	}
	return true
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
