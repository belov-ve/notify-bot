package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
    "strings"
    "time"
    "log/slog"
)

// MatrixLoginRequest – структура запроса для получения access_token по паролю.
// Используется стандартный метод m.login.password.
type MatrixLoginRequest struct {
    Type       string `json:"type"`
    Identifier struct {
        Type string `json:"type"`
        User string `json:"user"`
    } `json:"identifier"`
    Password string `json:"password"`
}

// MatrixLoginResponse – ответ сервера с access_token.
type MatrixLoginResponse struct {
    AccessToken string `json:"access_token"`
}

// matrixLogin выполняет аутентификацию на Matrix-сервере по паролю.
// Возвращает access_token или ошибку. Вызывается как при первом запуске,
// так и при повторной попытке после получения 401 (токен истёк) или сетевой ошибки.
func matrixLogin(homeserver, username, password string) (string, error) {
    url := fmt.Sprintf("%s/_matrix/client/v3/login", homeserver)
    req := MatrixLoginRequest{
        Type: "m.login.password",
    }
    req.Identifier.Type = "m.id.user"
    req.Identifier.User = username
    req.Password = password
    jsonData, _ := json.Marshal(req)

    resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return "", fmt.Errorf("login failed: %d", resp.StatusCode)
    }
    var loginResp MatrixLoginResponse
    if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
        return "", err
    }
    if loginResp.AccessToken == "" {
        return "", fmt.Errorf("login returned empty access token")
    }
    return loginResp.AccessToken, nil
}

// sendMatrixMessage отправляет одно сообщение в указанную комнату через Matrix Client-Server API.
// Использует PUT-запрос к /send/m.room.message с уникальным transaction ID (наносекунды).
// Возвращает ошибку, если статус ответа не 200 OK.
func sendMatrixMessage(homeserver, roomID, accessToken, text string) error {
    // Формируем уникальный transaction ID на основе текущего времени в наносекундах.
    // Это гарантирует, что даже при повторных отправках одного сообщения ID будут разными.
    url := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%d", homeserver, roomID, time.Now().UnixNano())
    payload := map[string]string{
        "msgtype": "m.text",
        "body":    text,
    }
    jsonData, _ := json.Marshal(payload)

    req, _ := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))
    req.Header.Set("Authorization", "Bearer "+accessToken)
    req.Header.Set("Content-Type", "application/json")

    client := &http.Client{Timeout: 10 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("matrix send error: %d", resp.StatusCode)
    }
    return nil
}

// sendMatrixWithRetry – основная функция отправки с повторными попытками и перелогином.
// Поддерживает два способа аутентификации: готовый access_token или пароль (тогда токен получается автоматически).
// ВАЖНО: логин выполняется внутри цикла повторных попыток, чтобы обрабатывать временные
// проблемы с сетью, DNS, недоступность сервера и т.д.
// При ошибке 401 (истёкший токен) выполняет повторный логин и продолжает попытки.
// Экспоненциальная задержка: delay = retryDelay * 2^(attempt-1).
func sendMatrixWithRetry(homeserver, roomID, accessToken, password, username, text string, retryCount, retryDelay int) error {
    // Гарантируем хотя бы одну попытку.
    if retryCount <= 0 {
        retryCount = 1
    }
    delay := time.Duration(retryDelay) * time.Second
    var token string
    var err error

    for attempt := 1; attempt <= retryCount; attempt++ {
        slog.Debug("Matrix attempt",
            "attempt", attempt,
            "max_attempts", retryCount,
            "room", roomID,
        )

        // Получаем токен (если его нет или он протух).
        // Логин выполняется на каждой попытке, если токен пуст.
        // Это позволяет обрабатывать временные ошибки DNS/сети при первом логине.
        if token == "" {
            if accessToken != "" {
                // Используем готовый access_token (не требует логина)
                token = accessToken
                slog.Debug("Matrix: using provided access token")
            } else if password != "" {
                // Пытаемся получить токен по паролю (может временно не работать из-за DNS)
                slog.Debug("Matrix: obtaining token via password", "username", username)
                token, err = matrixLogin(homeserver, username, password)
                if err != nil {
                    slog.Warn("Matrix login failed", "attempt", attempt, "error", err)
                    token = "" // сбрасываем, чтобы на следующей попытке повторить логин
                    if attempt == retryCount {
                        return fmt.Errorf("matrix login failed after %d attempts: %w", retryCount, err)
                    }
                    slog.Debug("Matrix retry delay", "delay_seconds", delay.Seconds())
                    time.Sleep(delay)
                    delay *= 2
                    continue // переходим к следующей попытке (логин повторится)
                }
                slog.Debug("Matrix: login successful, token obtained")
            } else {
                return fmt.Errorf("no access_token or password provided")
            }
        }

        // Отправляем сообщение с полученным токеном
        err = sendMatrixMessage(homeserver, roomID, token, text)
        if err == nil {
            slog.Info("Matrix message sent", "room", roomID)
            return nil
        }

        slog.Warn("Matrix send failed",
            "attempt", attempt,
            "error", err,
        )

		// Если ошибка авторизации (401 или unauthorized), сбрасываем токен – на следующей попытке выполним перелогин.
		if password != "" && (strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "unauthorized")) {
			slog.Debug("Matrix: token expired, will re-login on next attempt")
			token = ""
		}

        if attempt < retryCount {
            slog.Debug("Matrix retry delay", "delay_seconds", delay.Seconds())
            time.Sleep(delay)
            delay *= 2
        }
    }
    return fmt.Errorf("matrix send failed after %d attempts", retryCount)
}