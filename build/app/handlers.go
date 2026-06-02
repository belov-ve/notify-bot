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

	var message string
	var filePath, fileName, mimeType string

	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// [NEW] Обработка multipart/form-data для прямой отправки файлов и документов
		// Ограничиваем буфер парсинга формы в 20 МБ
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			logger.Error("Multipart form parse error", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()

		// Извлекаем текстовое описание из формы
		message = r.FormValue("text")

		// Извлекаем прикрепленный файл из поля "file"
		file, header, err := r.FormFile("file")
		if err == nil {
			defer file.Close()
			fileName = header.Filename

			// Определяем директорию для сохранения медиафайлов
			mediaDir := "/app/data/media"
			if envPath := os.Getenv("DB_PATH"); envPath != "" {
				mediaDir = filepath.Join(filepath.Dir(envPath), "media")
			}
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

			// Читаем первые 512 байт для автоматического определения MIME-типа
			buffer := make([]byte, 512)
			_, _ = file.Seek(0, io.SeekStart)
			n, _ := file.Read(buffer)
			mimeType = http.DetectContentType(buffer[:n])
		}
	} else {
		// [NEW] Обработка JSON-запроса (application/json) с поддержкой Base64 в поле image
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
			mediaDir := "/app/data/media"
			if envPath := os.Getenv("DB_PATH"); envPath != "" {
				mediaDir = filepath.Join(filepath.Dir(envPath), "media")
			}
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
	// Добавляем метку времени, если включено в настройках инстанса (ShowTime).
	if inst.ShowTime {
		timestamp := now.Local().Format("2006-01-02 15:04:05 MST")
		if message != "" {
			message = fmt.Sprintf("%s\n\n%s", message, timestamp)
		} else {
			message = timestamp
		}
	}

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