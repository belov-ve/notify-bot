package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// sendTelegramMessage отправляет текстовое сообщение или файл в Telegram через Bot API.
// Поддерживает отправку изображений через /sendPhoto и документов через /sendDocument.
// При превышении длины подписи (1024 символа) разбивает текст и досылает остаток вторым сообщением.
func sendTelegramMessage(botToken, chatID, text string, retryCount, retryDelay int, filePath, fileName, mimeType string) error {
	var url string
	var isMultipart bool
	var isImage bool

	if filePath != "" {
		isMultipart = true
		isImage = strings.HasPrefix(mimeType, "image/")
		if isImage {
			// [НОВОЕ] Проверяем размер файла на диске. Если он превышает лимит 10 МБ,
			// переключаемся на отправку файлом (sendDocument) во избежание ошибок API.
			if fileInfo, err := os.Stat(filePath); err == nil {
				if fileInfo.Size() > 10*1024*1024 { // 10 MB
					slog.Info("Image size exceeds 10MB limit, falling back to sending as document",
						"path", filePath, "size", fileInfo.Size())
					isImage = false
				}
			}
		}

		if isImage {
			url = fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", botToken)
		} else {
			url = fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", botToken)
		}
	} else {
		url = fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	}

	// Гарантируем хотя бы одну попытку, даже если в конфиге 0.
	if retryCount <= 0 {
		retryCount = 1
	}
	delay := time.Duration(retryDelay) * time.Second

	for attempt := 1; attempt <= retryCount; attempt++ {
		// Логируем начало попытки (полезно для отладки проблем с сетью или API)
		slog.Debug("Telegram attempt",
			"attempt", attempt,
			"max_attempts", retryCount,
			"chat_id", chatID,
			"multipart", isMultipart,
			"image", isImage,
		)

		var req *http.Request
		var reqErr error
		var writer *multipart.Writer

		if isMultipart {
			// [NEW] Готовим multipart/form-data тело для отправки файлов
			var requestBody bytes.Buffer
			writer = multipart.NewWriter(&requestBody)

			_ = writer.WriteField("chat_id", chatID)

			file, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("failed to open media file %s: %w", filePath, err)
			}

			fieldName := "document"
			if isImage {
				fieldName = "photo"
			}

			part, err := writer.CreateFormFile(fieldName, fileName)
			if err != nil {
				file.Close()
				return fmt.Errorf("failed to create multipart file field: %w", err)
			}

			if _, err = io.Copy(part, file); err != nil {
				file.Close()
				return fmt.Errorf("failed to copy media file content: %w", err)
			}
			file.Close()

			// Ограничиваем описание (caption) для медиа до 1024 символов
			captionText := text
			if len(text) > 1024 {
				captionText = text[:1024]
			}
			if captionText != "" {
				_ = writer.WriteField("caption", captionText)
			}

			if err = writer.Close(); err != nil {
				return fmt.Errorf("failed to close multipart writer: %w", err)
			}

			req, reqErr = http.NewRequest("POST", url, &requestBody)
			if reqErr == nil {
				req.Header.Set("Content-Type", writer.FormDataContentType())
			}
		} else {
			// Обычный JSON для текстового сообщения
			payload := map[string]string{
				"chat_id": chatID,
				"text":    text,
			}
			jsonData, _ := json.Marshal(payload)
			req, reqErr = http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
			if reqErr == nil {
				req.Header.Set("Content-Type", "application/json")
			}
		}

		if reqErr != nil {
			slog.Warn("Telegram request build failed", "attempt", attempt, "error", reqErr)
			if attempt == retryCount {
				return fmt.Errorf("telegram request build failed: %w", reqErr)
			}
			time.Sleep(delay)
			delay *= 2
			continue
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			// Ошибка на уровне HTTP (нет сети, таймаут, DNS и т.д.)
			slog.Warn("Telegram attempt failed",
				"attempt", attempt,
				"error", err,
			)
			if attempt == retryCount {
				return fmt.Errorf("telegram send failed after %d attempts: %w", retryCount, err)
			}
			slog.Debug("Telegram retry delay", "delay_seconds", delay.Seconds())
			time.Sleep(delay)
			delay *= 2
			continue
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close() // Закрываем тело ответа сразу же при успешном исходе
			slog.Info("Telegram message/file sent successfully", "chat_id", chatID)

			// [NEW] Если текст превышал 1024 символа при отправке с файлом, досылаем остаток текста отдельно
			if isMultipart && len(text) > 1024 {
				extraText := text[1024:]
				slog.Debug("Sending remaining text of long caption via separate message", "chat_id", chatID)
				if err := sendTelegramMessage(botToken, chatID, extraText, 1, retryDelay, "", "", ""); err != nil {
					slog.Warn("Failed to send remaining caption text for Telegram", "error", err)
				}
			}
			return nil
		}

		// Обработка ошибок API Telegram
		if resp.StatusCode == 401 {
			// 401 означает неверный токен бота – выводим маскированный токен для диагностики
			slog.Error("Telegram API error 401 (unauthorized)",
				"bot_token", maskToken(botToken),
				"chat_id", chatID,
				"attempt", attempt,
			)
		} else {
			slog.Warn("Telegram API error",
				"status", resp.StatusCode,
				"attempt", attempt,
			)
		}
		resp.Body.Close() // Закрываем тело ответа перед повторной попыткой, чтобы избежать утечек дескрипторов соединений

		if attempt < retryCount {
			slog.Debug("Telegram retry delay", "delay_seconds", delay.Seconds())
			time.Sleep(delay)
			delay *= 2
		}
	}
	return fmt.Errorf("telegram send failed after %d attempts", retryCount)
}