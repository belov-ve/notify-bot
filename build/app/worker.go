package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// wakeUpChan – канал для мгновенного пробуждения воркера.
var wakeUpChan = make(chan struct{}, 1)

// StartWorker запускает фоновый процесс обработки очереди.
func StartWorker(ctx context.Context, db *DBWrapper, cfg *Config, interval time.Duration) {
	slog.Info("Starting delivery worker", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

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
			db.DeleteMessage(msg.ID)
		} else {
			if isExpired {
				slog.Warn("Message expired after last attempt, removing from queue",
					"id", msg.ID, "deadline", msg.TTLDeadline)
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
		err = sendTelegramMessage(inst.Telegram.BotToken, inst.Telegram.ChatID, payload,
			inst.Telegram.RetryCount, inst.Telegram.RetryDelay)
	case "matrix":
		if inst.Matrix == nil || !inst.Matrix.Enabled {
			slog.Error("Matrix is disabled or not configured", "instance", inst.Name)
			return false
		}
		err = sendMatrixWithRetry(inst, payload)
	default:
		return false
	}

	if err != nil {
		slog.Error("Network send error", "service", msg.Service, "error", err, "id", msg.ID)
		return false
	}
	return true
}
