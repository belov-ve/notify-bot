package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// globalDB – глобальная переменная для доступа к БД из обработчиков.
var globalDB *DBWrapper

// globalConfig – актуальная конфигурация для получения параметров инстансов на лету.
var globalConfig *Config

// configMu – мьютекс для безопасного конкурентного чтения и записи globalConfig.
var configMu sync.RWMutex

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

	configMu.RLock()
	if globalDB == nil || globalConfig == nil {
		configMu.RUnlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	dbStats, err := globalDB.GetQueueStats()
	if err != nil {
		configMu.RUnlock()
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
	configMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fullStats)
}

// notifyHandler – основной эндпоинт /notify.
// Теперь он принимает имя инстанса и берет актуальные настройки из глобального конфига.
func notifyHandler(w http.ResponseWriter, r *http.Request, instanceName string) {
	configMu.RLock()
	if globalConfig == nil {
		configMu.RUnlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Получаем актуальный инстанс из конфига (позволяет менять настройки без рестарта порта).
	instPtr := globalConfig.GetInstanceByName(instanceName)
	if instPtr == nil || !instPtr.Enabled {
		configMu.RUnlock()
		slog.Warn("Request to disabled or missing instance", "instance", instanceName)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Создаем глубокую копию Instance, чтобы изменения конфигурации
	// (например, при hot-reload) не вызвали Data Race во время обработки запроса.
	instCopy := *instPtr
	if instPtr.Telegram != nil {
		tg := *instPtr.Telegram
		instCopy.Telegram = &tg
	}
	if instPtr.Matrix != nil {
		mtx := *instPtr.Matrix
		instCopy.Matrix = &mtx
	}
	inst := &instCopy
	configMu.RUnlock()

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

	var message string
	var filePath, fileName, mimeType string

	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// Handle multipart/form-data for direct file and document upload.
		// Ограничиваем буфер парсинга формы в 20 МБ
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			logger.Error("Multipart form parse error", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()

		// Извлекаем все текстовые поля из multipart-формы для формирования описания
		var lines []string
		if textVal := r.FormValue("text"); textVal != "" {
			lines = append(lines, textVal)
		}

		// Для сохранения красивого детерминированного порядка известных полей
		orderedKeys := []string{
			"ID события",
			"IP камеры",
			"MAC-адрес",
			"Время на камере",
			"Время шлюза",
			"Имя устройства",
			"Кадр",
			"Событие",
		}

		// Будем отслеживать уже добавленные в сообщение поля
		processedFields := make(map[string]bool)
		processedFields["text"] = true
		processedFields["file"] = true // Файл не является текстовым описанием

		for _, key := range orderedKeys {
			if vals, ok := r.MultipartForm.Value[key]; ok && len(vals) > 0 && vals[0] != "" {
				lines = append(lines, fmt.Sprintf("%s: %s", key, vals[0]))
				processedFields[key] = true
			}
		}

		// Добавляем все оставшиеся кастомные поля формы, если они переданы
		for key, vals := range r.MultipartForm.Value {
			if processedFields[key] {
				continue
			}
			if len(vals) > 0 && vals[0] != "" {
				lines = append(lines, fmt.Sprintf("%s: %s", key, vals[0]))
			}
		}

		message = strings.Join(lines, "\n")

		// Извлекаем прикрепленный файл из поля "file"
		file, header, err := r.FormFile("file")
		if err == nil {
			defer file.Close()
			fileName = header.Filename

			// Определяем директорию для сохранения медиафайлов
			mediaDir := getMediaDir()
			filePath = filepath.Join(mediaDir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), fileName))

			// Записываем файл на диск
			outFile, createErr := os.Create(filePath)
			if createErr != nil {
				logger.Error("Failed to create file on disk for upload", "error", createErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			// Копируем данные из буфера запроса в локальный файл
			_, copyErr := io.Copy(outFile, file)
			outFile.Close() // Закрываем дескриптор файла сразу же, чтобы сбросить буферы и разблокировать его перед записью в БД

			if copyErr != nil {
				logger.Error("Failed to copy uploaded file content", "error", copyErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			// Пытаемся извлечь MIME-тип из заголовка Content-Type части multipart формы
			mimeType = header.Header.Get("Content-Type")

			// Если MIME-тип не передан или неопределен, надежно определяем по байтам файла с диска
			if mimeType == "" || mimeType == "application/octet-stream" {
				diskFile, openErr := os.Open(filePath)
				if openErr == nil {
					// defer гарантирует закрытие в случае непредвиденных паник, закрываем вручную сразу после чтения
					defer diskFile.Close()
					buffer := make([]byte, 512)
					n, readErr := diskFile.Read(buffer)
					if readErr == nil && n > 0 {
						mimeType = http.DetectContentType(buffer[:n])
					} else if readErr != nil {
						logger.Warn("Failed to read file content for MIME detection", "path", filePath, "error", readErr)
					}
					diskFile.Close()
				}
			}
		}
	} else {
		// Handle JSON request (application/json) with Base64 image support.
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
		var imageBase64 string

		// Находим текстовое поле и Base64-данные картинки
		for _, pair := range data {
			if pair.Key == "text" {
				if val, ok := pair.Value.(string); ok {
					textFieldValue = val
					hasText = true
				}
			} else if pair.Key == "image" {
				if val, ok := pair.Value.(string); ok {
					imageBase64 = val
				}
			}
		}

		if hasText {
			lines = append(lines, textFieldValue)
		}

		// Формируем срез остальных полей (пропуская служебные "text" и "image")
		for _, pair := range data {
			if pair.Key == "text" || pair.Key == "image" {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s: %v", pair.Key, pair.Value))
		}
		message = strings.Join(lines, "\n")

		// Если передана Base64-картинка, сохраняем её локально
		if imageBase64 != "" {
			var rawDecoded []byte
			var decodeErr error

			// Обработка Data URI формата (например, data:image/png;base64,...)
			if strings.HasPrefix(imageBase64, "data:") && strings.Contains(imageBase64, ";base64,") {
				parts := strings.SplitN(imageBase64, ";base64,", 2)
				if len(parts) == 2 {
					mimeParts := strings.SplitN(parts[0], ":", 2)
					if len(mimeParts) == 2 {
						mimeType = mimeParts[1]
					}
					rawDecoded, decodeErr = base64.StdEncoding.DecodeString(parts[1])
				} else {
					decodeErr = fmt.Errorf("invalid Data URI format")
				}
			} else {
				// Обычный Base64 без префиксов
				rawDecoded, decodeErr = base64.StdEncoding.DecodeString(imageBase64)
			}

			fileName = fmt.Sprintf("image_%s.png", reqID)
			mediaDir := getMediaDir()
			filePath = filepath.Join(mediaDir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), fileName))

			if decodeErr != nil {
				// При ошибке парсинга картинки — создаем заглушку с красным крестом
				logger.Warn("Failed to decode base64 image, generating placeholder warning image", "error", decodeErr)
				if err := createPlaceholderImage(filePath); err != nil {
					logger.Error("Failed to generate placeholder image", "error", err)
					filePath = ""
					fileName = ""
					mimeType = ""
				} else {
					mimeType = "image/png"
				}
			} else {
				// Успешно декодировано: пишем файл на диск
				if err := os.WriteFile(filePath, rawDecoded, 0644); err != nil {
					logger.Error("Failed to write decoded image to disk", "error", err)
					filePath = ""
					fileName = ""
					mimeType = ""
				} else {
					// Если тип не определился из Data URI, определяем автоматически по байтам
					if mimeType == "" {
						mimeType = http.DetectContentType(rawDecoded)
					}
				}
			}
		}
	}

	now := time.Now()

	deadline := now.Add(time.Duration(inst.TTL) * time.Second)

	savedCount := 0
	// Сохраняем в очередь для Telegram
	if inst.Telegram != nil && inst.Telegram.Enabled {
		msg := &Message{
			InstanceName: inst.Name,
			Service:      "telegram",
			Payload:      message,
			Status:       "pending",
			TTLDeadline:  deadline,
			CreatedAt:    now,
			FilePath:     filePath,
			FileName:     fileName,
			MimeType:     mimeType,
		}
		if err := globalDB.SaveMessage(msg); err != nil {
			logger.Error("Database save error (Telegram)", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		WakeUpWorker(inst.Name, "telegram")
		savedCount++
	}

	// Сохраняем в очередь для Matrix
	if inst.Matrix != nil && inst.Matrix.Enabled {
		msg := &Message{
			InstanceName: inst.Name,
			Service:      "matrix",
			Payload:      message,
			Status:       "pending",
			TTLDeadline:  deadline,
			CreatedAt:    now,
			FilePath:     filePath,
			FileName:     fileName,
			MimeType:     mimeType,
		}
		if err := globalDB.SaveMessage(msg); err != nil {
			logger.Error("Database save error (Matrix)", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		WakeUpWorker(inst.Name, "matrix")
		savedCount++
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "reqID": reqID})
	logger.Info("Notification queued successfully", "ip", clientIP)
}