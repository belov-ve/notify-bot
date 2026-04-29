package main

import (
    "encoding/json"
    "fmt"
    "net"
    "net/http"
    "strings"
    "time"
    "log/slog"
    "sync"
)

// wg используется для graceful shutdown: дожидаемся завершения всех горутин отправки.
var wg sync.WaitGroup

// healthHandler – эндпоинт /health для проверки работоспособности экземпляра.
// Всегда возвращает 200 OK с JSON {"status":"ok"}.
// Логирует запрос на уровне DEBUG.
func healthHandler(w http.ResponseWriter, r *http.Request, instanceName string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
    slog.Debug("Health check", "instance", instanceName, "remote", r.RemoteAddr)
}

// notifyHandler – основной эндпоинт /notify. Обрабатывает POST-запросы с произвольным JSON.
// Выполняет:
//   - Генерацию короткого идентификатора запроса (reqID) для сквозного логирования.
//   - Проверку IP клиента по списку разрешённых сетей (allowed_ips).
//   - Парсинг JSON и формирование текстового сообщения.
//   - Асинхронную отправку в Telegram и/или Matrix с ограничением параллелизма (семафор).
//   - Немедленный ответ 202 Accepted.
func notifyHandler(w http.ResponseWriter, r *http.Request, inst *Instance) {
    // Генерация короткого ID запроса: берём последние 6 цифр из наносекунд.
    // Это даёт примерно 1 000 000 уникальных значений в секунду, достаточно для трейсинга.
    nanoStr := fmt.Sprintf("%d", time.Now().UnixNano())
    var reqID string
    if len(nanoStr) >= 6 {
        reqID = nanoStr[len(nanoStr)-6:]
    } else {
        reqID = nanoStr
    }

    // Локальный логгер с привязкой к reqID и имени инстанса – все последующие логи
    // будут автоматически содержать эти поля, что упрощает отслеживание цепочки обработки.
    logger := slog.With("reqID", reqID, "instance", inst.Name)

    clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
    logger.Debug("Request received", "ip", clientIP)

    // Проверка IP по разрешённым сетям.
    allowed := false
    for _, cidr := range inst.AllowedIPs {
        ipnet, err := parseCIDR(cidr)
        if err != nil {
            slog.Error("Invalid CIDR", "cidr", cidr, "instance", inst.Name)
            continue
        }
        if ipnet.Contains(net.ParseIP(clientIP)) {
            allowed = true
            break
        }
    }
    if !allowed {
        logger.Warn("Blocked IP", "ip", clientIP)
        // Формат ответа совпадает с Python-версией (текст/html с кодом 403)
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.WriteHeader(http.StatusForbidden)
        w.Write([]byte("Forbidden\n<p>You don't have the permission to access the requested resource.</p>\n"))
        return
    }

    // Парсинг JSON-тела запроса.
    var data map[string]interface{}
    if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
        logger.Error("Invalid JSON", "error", err)
        w.WriteHeader(http.StatusBadRequest)
        return
    }
    defer r.Body.Close() // Явное закрытие тела – хорошая практика.

    logger.Debug("Request JSON", "data", data)

    // Формирование сообщения: если есть поле "text", оно становится первой строкой.
    // Все остальные поля выводятся в формате "ключ: значение".
    var lines []string
    if text, ok := data["text"].(string); ok {
        lines = append(lines, text)
        delete(data, "text") // Удаляем, чтобы не дублировать.
    }
    for k, v := range data {
        lines = append(lines, fmt.Sprintf("%s: %v", k, v))
    }
    message := strings.Join(lines, "\n")
    logger.Debug("Formatted message", "message", message)

    // Вспомогательная функция для запуска горутин с ограничением параллелизма.
    // Мы используем семафор (буферизированный канал) и WaitGroup.
    runWithLimit := func(fn func()) {
        wg.Add(1)
        go func() {
            defer wg.Done()
            semaphore <- struct{}{}        // Захватить слот (блокируется, если все слоты заняты)
            defer func() { <-semaphore }() // Освободить слот после завершения
            fn()
        }()
    }

    // Асинхронная отправка в Telegram (если включён).
    if inst.Telegram != nil && inst.Telegram.Enabled {
        runWithLimit(func() {
            err := sendTelegramMessage(inst.Telegram.BotToken, inst.Telegram.ChatID, message,
                inst.Telegram.RetryCount, inst.Telegram.RetryDelay)
            if err != nil {
                logger.Error("Telegram send error", "error", err)
            }
        })
    }

    // Асинхронная отправка в Matrix (если включён).
    if inst.Matrix != nil && inst.Matrix.Enabled {
        runWithLimit(func() {
            err := sendMatrixWithRetry(inst.Matrix.Homeserver, inst.Matrix.RoomID,
                inst.Matrix.AccessToken, inst.Matrix.Password, inst.Matrix.Username,
                message, inst.Matrix.RetryCount, inst.Matrix.RetryDelay)
            if err != nil {
                logger.Error("Matrix send error", "error", err)
            }
        })
    }

    // Отвечаем клиенту, что запрос принят в обработку.
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusAccepted)
    json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
    logger.Info("Request accepted", "ip", clientIP)
}