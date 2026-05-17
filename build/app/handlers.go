package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// globalDB – глобальная переменная для доступа к БД из обработчиков.
var globalDB *DBWrapper

// globalConfig – актуальная конфигурация для получения параметров инстансов на лету.
var globalConfig *Config

// healthHandler – эндпоинт /health для проверки работоспособности экземпляра.
func healthHandler(w http.ResponseWriter, r *http.Request, instanceName string) {
	slog.Debug("Instance health check request", "instance", instanceName, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "instance": instanceName})
}

// statsHandler – эндпоинт /stats для получения статистики очередей по всем инстансам.
func statsHandler(w http.ResponseWriter, r *http.Request) {
	slog.Debug("Queue stats requested", "remote", r.RemoteAddr)

	if globalDB == nil || globalConfig == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	dbStats, err := globalDB.GetQueueStats()
	if err != nil {
		slog.Error("Failed to fetch queue stats from DB", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	fullStats := make(map[string]int)
	for _, inst := range globalConfig.Instances {
		if inst.Enabled {
			count := 0
			if val, ok := dbStats[inst.Name]; ok {
				count = val
			}
			fullStats[inst.Name] = count
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fullStats)
}

// notifyHandler – основной эндпоинт /notify.
// Теперь он принимает имя инстанса и берет актуальные настройки из глобального конфига.
func notifyHandler(w http.ResponseWriter, r *http.Request, instanceName string) {
	if globalConfig == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Получаем актуальный инстанс из конфига (позволяет менять настройки без рестарта порта).
	inst := globalConfig.GetInstanceByName(instanceName)
	if inst == nil || !inst.Enabled {
		slog.Warn("Request to disabled or missing instance", "instance", instanceName)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	nanoStr := fmt.Sprintf("%d", time.Now().UnixNano())
	var reqID string
	if len(nanoStr) >= 6 {
		reqID = nanoStr[len(nanoStr)-6:]
	} else {
		reqID = nanoStr
	}

	logger := slog.With("reqID", reqID, "instance", inst.Name)
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	logger.Debug("Request received", "ip", clientIP)

	// Проверка IP.
	allowed := false
	for _, cidr := range inst.AllowedIPs {
		ipnet, err := parseCIDR(cidr)
		if err != nil {
			slog.Error("Invalid CIDR in config", "cidr", cidr, "instance", inst.Name)
			continue
		}
		if ipnet.Contains(net.ParseIP(clientIP)) {
			allowed = true
			break
		}
	}
	if !allowed {
		logger.Warn("Access denied: blocked IP", "ip", clientIP)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("Forbidden\n<p>You don't have the permission to access the requested resource.</p>\n"))
		return
	}

	// Используем кастомный декодер для сохранения порядка полей.
	data, err := DecodeOrderedJSON(r.Body)
	if err != nil {
		logger.Error("JSON parse error", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var lines []string
	var textFieldValue string
	var hasText bool

	// 1. Сначала ищем поле "text", чтобы вывести его первым.
	for _, pair := range data {
		if pair.Key == "text" {
			if val, ok := pair.Value.(string); ok {
				textFieldValue = val
				hasText = true
			}
			break
		}
	}

	if hasText {
		lines = append(lines, textFieldValue)
	}

	// 2. Выводим остальные поля в оригинальном порядке.
	for _, pair := range data {
		if pair.Key == "text" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %v", pair.Key, pair.Value))
	}
	message := strings.Join(lines, "\n")

	now := time.Now()
	// 3. Добавляем метку времени, если включено в конфиге профиля (ShowTime).
	// Отделяем одной пустой строкой (\n\n).
	if inst.ShowTime {
		timestamp := now.Local().Format("2006-01-02 15:04:05 MST")
		message = fmt.Sprintf("%s\n\n%s", message, timestamp)
	}

	deadline := now.Add(time.Duration(inst.TTL) * time.Second)

	savedCount := 0
	// Сохраняем для Telegram.
	if inst.Telegram != nil && inst.Telegram.Enabled {
		msg := &Message{
			InstanceName: inst.Name,
			Service:      "telegram",
			Payload:      message,
			Status:       "pending",
			TTLDeadline:  deadline,
			CreatedAt:    now,
		}
		if err := globalDB.SaveMessage(msg); err != nil {
			logger.Error("Database save error (Telegram)", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		savedCount++
	}

	// Сохраняем для Matrix.
	if inst.Matrix != nil && inst.Matrix.Enabled {
		msg := &Message{
			InstanceName: inst.Name,
			Service:      "matrix",
			Payload:      message,
			Status:       "pending",
			TTLDeadline:  deadline,
			CreatedAt:    now,
		}
		if err := globalDB.SaveMessage(msg); err != nil {
			logger.Error("Database save error (Matrix)", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		savedCount++
	}

	if savedCount > 0 {
		select {
		case wakeUpChan <- struct{}{}:
		default:
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "reqID": reqID})
	logger.Info("Notification queued successfully", "ip", clientIP)
}