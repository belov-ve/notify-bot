package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
    "log/slog"
)

// sendTelegramMessage отправляет текстовое сообщение в Telegram через Bot API.
// Реализует экспоненциальную задержку при повторных попытках (задержка удваивается после каждой неудачи).
// При ошибке 401 (неверный токен) выводит в лог маскированный токен для диагностики.
// Логирует каждую попытку на уровне DEBUG, что помогает отслеживать процесс.
func sendTelegramMessage(botToken, chatID, text string, retryCount, retryDelay int) error {
    url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
    payload := map[string]string{
        "chat_id": chatID,
        "text":    text,
    }
    jsonData, _ := json.Marshal(payload)
    delay := time.Duration(retryDelay) * time.Second

    for attempt := 1; attempt <= retryCount; attempt++ {
        // Логируем начало попытки (полезно для отладки проблем с сетью или API)
        slog.Debug("Telegram attempt",
            "attempt", attempt,
            "max_attempts", retryCount,
            "chat_id", chatID,
        )

        resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
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
        defer resp.Body.Close()

        if resp.StatusCode == http.StatusOK {
            slog.Info("Telegram message sent", "chat_id", chatID)
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

        if attempt < retryCount {
            slog.Debug("Telegram retry delay", "delay_seconds", delay.Seconds())
            time.Sleep(delay)
            delay *= 2
        }
    }
    return fmt.Errorf("telegram send failed after %d attempts", retryCount)
}