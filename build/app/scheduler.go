package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// copyFile копирует файл из источника в место назначения.
// Используется для переноса файлов отчетов во временную директорию.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("failed to copy content: %w", err)
	}

	return out.Sync()
}

// executeCronTask выполняет конкретную cron-задачу (скрипт или HTTP GET)
// и регистрирует результат в SQLite очереди гарантированной доставки.
func (sm *ServerManager) executeCronTask(inst *Instance, item TaskItem) {
	slog.Info("Executing scheduled task", "task", item.Name, "instance", inst.Name)

	// Определяем отображаемое имя задачи для вывода в сообщениях чата.
	// Если задано описание (Description), выводим его в кавычках.
	// В противном случае используем техническое имя задачи (Name).
	taskDisplayName := item.Name
	if item.Description != "" {
		taskDisplayName = fmt.Sprintf("\"%s\"", item.Description)
	}

	var outputBytes []byte
	var err error

	// 1. Выполнение локального скрипта (имеет приоритет над URL)
	if item.Script != "" {
		scriptPath := filepath.Join("/app/scripts", item.Script)
		slog.Debug("Running task script", "task", item.Name, "path", scriptPath)

		if _, statErr := os.Stat(scriptPath); os.IsNotExist(statErr) {
			err = fmt.Errorf("script file does not exist: %s", scriptPath)
		} else {
			// Чтение таймаута для скриптов из SCRIPT_TIMEOUT (дефолт 15 секунд)
			timeoutSec := 15
			if envVal := os.Getenv("SCRIPT_TIMEOUT"); envVal != "" {
				if parsed, e := strconv.Atoi(envVal); e == nil && parsed > 0 {
					timeoutSec = parsed
				}
			}
			timeout := time.Duration(timeoutSec) * time.Second
			slog.Debug("Executing script with timeout", "task", item.Name, "timeout", timeout)

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			// Запускаем скрипт через bash (установленный в Docker-образе), чтобы поддерживать
			// bash-специфичные конструкции и синтаксис (например, echo -e)
			cmd := exec.CommandContext(ctx, "bash", scriptPath)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			runErr := cmd.Run()
			if runErr != nil {
				err = fmt.Errorf("script execution failed: %v, stderr: %s", runErr, stderr.String())
			} else {
				outputBytes = stdout.Bytes()
			}
		}
	} else if item.URL != "" {
		// 2. Выполнение HTTP GET запроса
		slog.Debug("Running task URL request", "task", item.Name, "url", item.URL)

		// Чтение таймаута для HTTP-запросов из HTTP_REQUEST_TIMEOUT (дефолт 15 секунд)
		timeoutSec := 15
		if envVal := os.Getenv("HTTP_REQUEST_TIMEOUT"); envVal != "" {
			if parsed, e := strconv.Atoi(envVal); e == nil && parsed > 0 {
				timeoutSec = parsed
			}
		}
		timeout := time.Duration(timeoutSec) * time.Second
		slog.Debug("Executing HTTP request with timeout", "task", item.Name, "timeout", timeout)

		httpClient := &http.Client{
			Timeout: timeout,
		}

		resp, httpErr := httpClient.Get(item.URL)
		if httpErr != nil {
			err = fmt.Errorf("HTTP GET failed: %v", httpErr)
		} else {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				// Ограничение чтения тела ответа до 1 МБ (как в меню)
				limitReader := io.LimitReader(resp.Body, 1024*1024)
				outputBytes, _ = io.ReadAll(limitReader)
			} else {
				err = fmt.Errorf("HTTP GET returned non-successful status: %s", resp.Status)
			}
		}
	} else {
		err = fmt.Errorf("neither script nor URL configured for task")
	}

	var message string
	var filePath, fileName, mimeType string

	// 3. Формирование ответа в зависимости от результата выполнения
	if err != nil {
		slog.Error("Scheduled task execution failed", "task", item.Name, "instance", inst.Name, "error", err)
		message = fmt.Sprintf("❌ Ошибка при выполнении задания %s", taskDisplayName)
	} else {
		// Успешный запуск, анализируем полученный stdout/body
		if len(outputBytes) > 0 && strings.TrimSpace(string(outputBytes)) != "" {
			// Проверяем, текстовый ли вывод
			if isTextContent("", outputBytes) {
				isJSONFileResult := false

				// Проверяем, не вернул ли скрипт JSON с путем к файлу (file_path / file)
				var fileJSON struct {
					FilePath string `json:"file_path"`
					File     string `json:"file"`
					Text     string `json:"text"`
					Caption  string `json:"caption"`
				}

				if jsonErr := json.Unmarshal(outputBytes, &fileJSON); jsonErr == nil {
					pathToCheck := fileJSON.FilePath
					if pathToCheck == "" {
						pathToCheck = fileJSON.File
					}
					if pathToCheck != "" {
						// Скрипт вернул путь к файлу. Проверяем его существование на диске
						if _, statErr := os.Stat(pathToCheck); statErr == nil || !os.IsNotExist(statErr) {
							isJSONFileResult = true

							// Копируем файл во временную директорию медиа.
							// Это дает монопольное право на файл боту для безопасного удаления после доставки во все каналы.
							tempDir := getMediaDir()
							_ = os.MkdirAll(tempDir, 0755)

							fileName = filepath.Base(pathToCheck)
							tempFilePath := filepath.Join(tempDir, fmt.Sprintf("task_%s_%d_%s", item.Name, time.Now().UnixNano(), fileName))

							if copyErr := copyFile(pathToCheck, tempFilePath); copyErr == nil {
								filePath = tempFilePath

								// Удаляем исходный файл, созданный скриптом, чтобы не засорять диск (так как бот
								// успешно скопировал его во временную папку и берет управление доставкой на себя)
								if rmErr := os.Remove(pathToCheck); rmErr != nil {
									slog.Warn("Failed to remove original task output file", "path", pathToCheck, "error", rmErr)
								}

								// Определяем MIME-тип файла по его начальным байтам
								mimeType = "application/octet-stream"
								if f, openErr := os.Open(tempFilePath); openErr == nil {
									buf := make([]byte, 512)
									if n, rErr := f.Read(buf); rErr == nil && n > 0 {
										mimeType = http.DetectContentType(buf[:n])
									}
									f.Close()
								}

								// Текст подписи берем из JSON
								msgText := fileJSON.Text
								if msgText == "" {
									msgText = fileJSON.Caption
								}
								if msgText != "" {
									message = fmt.Sprintf("✅ Задание %s выполнено успешно:\n%s", taskDisplayName, msgText)
								} else {
									message = fmt.Sprintf("✅ Результат задания %s (файл)", taskDisplayName)
								}
							} else {
								slog.Error("Failed to copy task output file to temp storage", "task", item.Name, "error", copyErr)
								message = fmt.Sprintf("❌ Ошибка при обработке файла задания %s", taskDisplayName)
								// Оставляем isJSONFileResult = true, чтобы не переходить к форматированию обычного текста
								// и сохранить сообщение об ошибке копирования.
							}
						} else {
							slog.Warn("Task output JSON contained file path, but file does not exist", "task", item.Name, "path", pathToCheck)
						}
					}
				}

				if !isJSONFileResult {
					// Обычный текстовый вывод. Форматируем JSON/XML аналогично меню.
					// Если вывод содержит HTML-тег <html>, то отправляем его без форматирования структуры.
					if strings.HasPrefix(strings.TrimSpace(string(outputBytes)), "<html>") {
						content := strings.TrimSpace(string(outputBytes))
						content = strings.TrimPrefix(content, "<html>")
						content = strings.TrimSuffix(content, "</html>")
						message = fmt.Sprintf("<html>✅ Задание %s выполнено успешно:\n%s</html>", taskDisplayName, truncateText(content, 4000))
					} else if pairs, decErr := DecodeOrderedJSON(bytes.NewReader(outputBytes)); decErr == nil {
						formatted := formatOrderedJSONValue(pairs, 0)
						message = fmt.Sprintf("✅ Задание %s выполнено успешно:\n%s", taskDisplayName, truncateText(formatted, 4000))
					} else {
						var jsonVal interface{}
						if jsonErr := json.Unmarshal(outputBytes, &jsonVal); jsonErr == nil {
							switch concreteVal := jsonVal.(type) {
							case []interface{}:
								formatted := formatAnyJSONValue(concreteVal, 0)
								message = fmt.Sprintf("✅ Задание %s выполнено успешно:\n%s", taskDisplayName, truncateText(formatted, 4000))
							default:
								message = fmt.Sprintf("✅ Задание %s выполнено успешно:\n%s", taskDisplayName, truncateText(string(outputBytes), 4000))
							}
						} else if xmlVal, xmlErr := parseXML(outputBytes); xmlErr == nil {
							stripped := stripXMLRoot(xmlVal)
							formatted := formatJSONValue(stripped, 0)
							message = fmt.Sprintf("✅ Задание %s выполнено успешно:\n%s", taskDisplayName, truncateText(formatted, 4000))
						} else {
							message = fmt.Sprintf("✅ Задание %s выполнено успешно:\n%s", taskDisplayName, truncateText(string(outputBytes), 4000))
						}
					}
				}
			} else {
				// Бинарный stdout (например, бинарник PNG картинки).
				// Сохраняем в файл, определяем тип и отправляем.
				tempDir := getMediaDir()
				_ = os.MkdirAll(tempDir, 0755)

				mimeType = http.DetectContentType(outputBytes)
				ext := ".bin"
				if strings.Contains(mimeType, "image/jpeg") {
					ext = ".jpg"
				} else if strings.Contains(mimeType, "image/png") {
					ext = ".png"
				} else if strings.Contains(mimeType, "image/gif") {
					ext = ".gif"
				}

				fileName = fmt.Sprintf("task_%s_%d%s", item.Name, time.Now().UnixNano(), ext)
				filePath = filepath.Join(tempDir, fileName)

				if writeErr := os.WriteFile(filePath, outputBytes, 0644); writeErr != nil {
					slog.Error("Failed to save binary stdout of task to file", "task", item.Name, "error", writeErr)
					message = fmt.Sprintf("❌ Ошибка при выполнении задания %s (сбой записи файла)", taskDisplayName)
					filePath = ""
					fileName = ""
					mimeType = ""
				} else {
					message = fmt.Sprintf("✅ Результат задания %s (бинарный вывод)", taskDisplayName)
				}
			}
		} else {
			// Вывод пуст
			message = fmt.Sprintf("✅ Задание %s выполнено успешно (вывод пуст)", taskDisplayName)
		}
	}

	// 4. Гарантированная доставка и TTL.
	// Результат сохраняется в Outbox SQLite как новое сообщение инстанса.
	now := time.Now()

	// Добавляем метку времени к сообщению, если это включено для инстанса (ShowTime)
	if inst.ShowTime {
		timestamp := now.Local().Format("2006-01-02 15:04:05 MST")
		message = fmt.Sprintf("%s\n\n%s", message, timestamp)
	}

	// Рассчитываем дедлайн по TTL. Если TTL <= 0, используем 24 часа по умолчанию для очереди
	deadline := now.Add(24 * time.Hour)
	if inst.TTL > 0 {
		deadline = now.Add(time.Duration(inst.TTL) * time.Second)
	}

	// Создаем сообщения для каждого включенного канала инстанса
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
		if dbErr := globalDB.SaveMessage(msg); dbErr != nil {
			slog.Error("Failed to save scheduled task output to DB (Telegram)", "task", item.Name, "error", dbErr)
		} else {
			slog.Debug("Scheduled task output queued for Telegram", "task", item.Name)
			WakeUpWorker(inst.Name, "telegram")
		}
	}

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
		if dbErr := globalDB.SaveMessage(msg); dbErr != nil {
			slog.Error("Failed to save scheduled task output to DB (Matrix)", "task", item.Name, "error", dbErr)
		} else {
			slog.Debug("Scheduled task output queued for Matrix", "task", item.Name)
			WakeUpWorker(inst.Name, "matrix")
		}
	}
}
