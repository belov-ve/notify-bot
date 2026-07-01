package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TelegramUser описывает пользователя в API Telegram.
type TelegramUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

// TelegramChat описывает чат в API Telegram.
type TelegramChat struct {
	ID int64 `json:"id"`
}

// TelegramMessage описывает входящее сообщение в API Telegram.
type TelegramMessage struct {
	MessageID int64         `json:"message_id"`
	Chat      TelegramChat  `json:"chat"`
	Text      string        `json:"text,omitempty"`
	From      *TelegramUser `json:"from,omitempty"`
}

// TelegramCallbackQuery описывает событие callback_query (нажатие inline-кнопки).
type TelegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    TelegramUser     `json:"from"`
	Message *TelegramMessage `json:"message,omitempty"`
	Data    string           `json:"data"`
}

// TelegramUpdate представляет входящее обновление от getUpdates.
type TelegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *TelegramMessage       `json:"message,omitempty"`
	EditedMessage *TelegramMessage       `json:"edited_message,omitempty"`
	CallbackQuery *TelegramCallbackQuery `json:"callback_query,omitempty"`
}

// InlineKeyboardButton описывает кнопку Inline клавиатуры.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// InlineKeyboardMarkup описывает разметку Inline клавиатуры.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// sendTelegramMessage отправляет текстовое сообщение или файл в Telegram через Bot API.
// Поддерживает отправку изображений через /sendPhoto и документов через /sendDocument.
// При превышении длины подписи (1024 символа) разбивает текст и досылает остаток вторым сообщением.
func sendTelegramMessage(botToken, chatID, text string, retryCount, retryDelay int, filePath, fileName, mimeType string) error {
	// Оборачиваем вызов sendTelegramMessage в sendTelegramMessageWithMarkup с nil клавиатурой
	return sendTelegramMessageWithMarkup(botToken, chatID, text, nil, retryCount, retryDelay, filePath, fileName, mimeType)
}

// sendTelegramMessageWithMarkup отправляет текстовое сообщение или файл в Telegram с поддержкой Inline клавиатуры.
func sendTelegramMessageWithMarkup(botToken, chatID, text string, replyMarkup *InlineKeyboardMarkup, retryCount, retryDelay int, filePath, fileName, mimeType string) error {
	var url string
	var isMultipart bool
	var isImage bool

	if filePath != "" {
		isMultipart = true
		isImage = strings.HasPrefix(mimeType, "image/")
		if isImage {
			// Проверяем размер файла на диске. Если он превышает лимит 10 МБ,
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
		// Логируем начало попытки
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
			// Prepare multipart/form-data body for file upload
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

			// Ограничиваем описание (caption) для медиа до 1024 символов (UTF-8 безопасно)
			captionText := text
			if len([]rune(text)) > 1024 {
				captionText = string([]rune(text)[:1024])
			}
			if captionText != "" {
				_ = writer.WriteField("caption", captionText)
			}

			// Добавляем reply_markup в multipart, если передан
			if replyMarkup != nil {
				markupData, _ := json.Marshal(replyMarkup)
				_ = writer.WriteField("reply_markup", string(markupData))
			}

			if err = writer.Close(); err != nil {
				return fmt.Errorf("failed to close multipart writer: %w", err)
			}

			req, reqErr = http.NewRequest("POST", url, &requestBody)
			if reqErr == nil {
				req.Header.Set("Content-Type", writer.FormDataContentType())
			}
		} else {
			// Обычный JSON для текстового сообщения с поддержкой reply_markup
			payload := map[string]interface{}{
				"chat_id": chatID,
				"text":    text,
			}
			if replyMarkup != nil {
				payload["reply_markup"] = replyMarkup
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
			slog.Warn("Telegram attempt failed",
				"attempt", attempt,
				"error", err,
			)
			if attempt == retryCount {
				return fmt.Errorf("telegram send failed after %d attempts: %w", retryCount, err)
			}
			time.Sleep(delay)
			delay *= 2
			continue
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			slog.Info("Telegram message/file sent successfully", "chat_id", chatID)

			if isMultipart && len([]rune(text)) > 1024 {
				extraText := string([]rune(text)[1024:])
				slog.Debug("Sending remaining text of long caption via separate message", "chat_id", chatID)
				if err := sendTelegramMessage(botToken, chatID, extraText, 1, retryDelay, "", "", ""); err != nil {
					slog.Error("Failed to send remaining caption text for Telegram", "error", err)
				}
			}
			return nil
		}

		if resp.StatusCode == 401 {
			slog.Error("Telegram API error 401 (unauthorized)",
				"bot_token", maskToken(botToken),
				"chat_id", chatID,
				"attempt", attempt,
			)
		} else {
			bodyBytes, _ := io.ReadAll(resp.Body)
			slog.Warn("Telegram API error",
				"status", resp.StatusCode,
				"body", string(bodyBytes),
				"attempt", attempt,
			)
		}
		resp.Body.Close()

		if attempt < retryCount {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return fmt.Errorf("telegram send failed after %d attempts", retryCount)
}

type telegramPollEntry struct {
	cancel context.CancelFunc
	token  string
}

var (
	telegramPollCancels        = make(map[string]telegramPollEntry) // instance name -> entry
	telegramPollMu             sync.Mutex
	executedTelegramCommands   = make(map[string]time.Time) // Кэш выполненных команд в формате "chat_id:message_id:command_name" -> время выполнения
	executedTelegramCommandsMu sync.Mutex
)

// checkAndMarkTelegramExecuted проверяет, выполнялась ли уже конкретная команда для данного сообщения.
// Возвращает true, если команда уже выполнялась.
func checkAndMarkTelegramExecuted(chatID int64, messageID int64, cmdName string) bool {
	executedTelegramCommandsMu.Lock()
	defer executedTelegramCommandsMu.Unlock()

	key := fmt.Sprintf("%d:%d:%s", chatID, messageID, cmdName)
	if _, exists := executedTelegramCommands[key]; exists {
		return true
	}

	// Очищаем старые записи (> 24 часов) для предотвращения утечки памяти
	now := time.Now()
	for k, t := range executedTelegramCommands {
		if now.Sub(t) > 24*time.Hour {
			delete(executedTelegramCommands, k)
		}
	}

	executedTelegramCommands[key] = now
	return false
}


// StopTelegramPolling отменяет контекст опроса для указанного инстанса и удаляет его из реестра.
func StopTelegramPolling(instanceName string) {
	telegramPollMu.Lock()
	defer telegramPollMu.Unlock()
	if entry, ok := telegramPollCancels[instanceName]; ok {
		slog.Info("Stopping Telegram polling loop", "instance", instanceName)
		entry.cancel()
		delete(telegramPollCancels, instanceName)
	}
}

// StopAllTelegramPolling останавливает все запущенные циклы опроса Telegram (используется при graceful shutdown).
func StopAllTelegramPolling() {
	telegramPollMu.Lock()
	defer telegramPollMu.Unlock()
	for name, entry := range telegramPollCancels {
		slog.Info("Stopping Telegram polling loop during shutdown", "instance", name)
		entry.cancel()
	}
	telegramPollCancels = make(map[string]telegramPollEntry)
}

// InitializeTelegramSyncClients запускает бесконечный цикл Long Polling для прослушивания входящих обновлений Telegram.
func InitializeTelegramSyncClients(cfg *Config) {
	telegramPollMu.Lock()
	defer telegramPollMu.Unlock()

	// Собираем уже запущенные токены, чтобы не допустить дублирования опроса на один токен
	activeTokens := make(map[string]string) // token -> instance name
	for name, entry := range telegramPollCancels {
		activeTokens[entry.token] = name
	}

	for i := range cfg.Instances {
		inst := &cfg.Instances[i]
		if inst.Enabled && inst.Telegram != nil && inst.Telegram.Enabled && inst.Telegram.Menu != "" {
			token := inst.Telegram.BotToken
			// Запускаем опрос только если он ещё не запущен для этого инстанса
			if _, running := telegramPollCancels[inst.Name]; !running {
				// Проверяем, не запущен ли уже опрос для этого же токена в другом инстансе
				if conflictInstance, exists := activeTokens[token]; exists {
					slog.Warn("Telegram polling client for this token is already running in another instance", 
						"token", maskToken(token), 
						"current_instance", inst.Name, 
						"running_instance", conflictInstance)
					continue
				}

				slog.Info("Starting Telegram polling client", "instance", inst.Name)
				ctx, cancel := context.WithCancel(context.Background())
				telegramPollCancels[inst.Name] = telegramPollEntry{
					cancel: cancel,
					token:  token,
				}
				activeTokens[token] = inst.Name
				go pollTelegramUpdates(ctx, inst)
			} else {
				slog.Debug("Telegram polling client already running", "instance", inst.Name)
			}
		} else {
			// Если опрос запущен, но в новой конфигурации он не должен работать, останавливаем его
			if entry, running := telegramPollCancels[inst.Name]; running {
				slog.Info("Stopping Telegram polling client (no longer required)", "instance", inst.Name)
				entry.cancel()
				delete(telegramPollCancels, inst.Name)
			}
		}
	}
}

// pollTelegramUpdates инициализирует и запускает цикл опроса.
func pollTelegramUpdates(ctx context.Context, inst *Instance) {
	botToken := inst.Telegram.BotToken
	menuID := inst.Telegram.Menu

	slog.Info("Starting Telegram long polling loop", "instance", inst.Name, "menu", menuID)

	// Сброс webhook при запуске, чтобы избежать ошибки 409 Conflict, если ранее был зарегистрирован webhook.
	deleteWebhookUrl := fmt.Sprintf("https://api.telegram.org/bot%s/deleteWebhook", botToken)
	req, err := http.NewRequestWithContext(ctx, "POST", deleteWebhookUrl, nil)
	if err == nil {
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			slog.Debug("Telegram webhook deleted successfully on start", "instance", inst.Name)
		} else {
			if ctx.Err() == nil {
				slog.Warn("Failed to delete Telegram webhook on start", "instance", inst.Name, "error", err)
			}
		}
	}

	// Очищаем старые накопившиеся обновления, чтобы бот не обрабатывал лавину старых команд при запуске.
	// Используем NewRequestWithContext для корректной отмены при завершении.
	clearUrl := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=-1&limit=1&timeout=0", botToken)
	clearReq, err := http.NewRequestWithContext(ctx, "GET", clearUrl, nil)
	if err == nil {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(clearReq)
		if err == nil {
			var clearRes struct {
				Ok     bool             `json:"ok"`
				Result []TelegramUpdate `json:"result"`
			}
			if decodeErr := json.NewDecoder(resp.Body).Decode(&clearRes); decodeErr == nil && len(clearRes.Result) > 0 {
				offset := clearRes.Result[0].UpdateID + 1
				slog.Debug("Telegram old updates cleared", "instance", inst.Name, "next_offset", offset)
				resp.Body.Close()
				runPollingLoop(ctx, inst, offset)
				return
			}
			resp.Body.Close()
		} else {
			if ctx.Err() == nil {
				slog.Warn("Failed to clear old Telegram updates", "instance", inst.Name, "error", err)
			}
		}
	}

	if ctx.Err() != nil {
		slog.Info("Telegram polling loop stopped before start (context cancelled)", "instance", inst.Name)
		return
	}

	runPollingLoop(ctx, inst, 0)
}

// runPollingLoop опрашивает Telegram API методом getUpdates в бесконечном цикле.
func runPollingLoop(ctx context.Context, inst *Instance, startOffset int64) {
	botToken := inst.Telegram.BotToken
	offset := startOffset
	client := &http.Client{Timeout: 40 * time.Second}

	for {
		// Проверяем отмену контекста перед началом новой итерации лонг-поллинга.
		select {
		case <-ctx.Done():
			slog.Info("Telegram polling loop stopped (context cancelled)", "instance", inst.Name)
			return
		default:
		}

		url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", botToken, offset)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			slog.Error("Failed to create Telegram getUpdates request", "instance", inst.Name, "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("Telegram polling loop stopped (context cancelled during request)", "instance", inst.Name)
				return
			}
			slog.Warn("Telegram getUpdates request failed, retrying...", "instance", inst.Name, "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			// Читаем тело ответа для детального логирования причин ошибки (например, деталей 409 Conflict)
			bodyBytes, _ := io.ReadAll(resp.Body)
			slog.Error("Telegram getUpdates returned error status", "instance", inst.Name, "status", resp.Status, "body", string(bodyBytes))
			resp.Body.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		var updateResponse struct {
			Ok     bool             `json:"ok"`
			Result []TelegramUpdate `json:"result"`
		}

		err = json.NewDecoder(resp.Body).Decode(&updateResponse)
		resp.Body.Close()
		if err != nil {
			slog.Error("Failed to decode Telegram updates JSON", "instance", inst.Name, "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if !updateResponse.Ok {
			slog.Error("Telegram getUpdates ok=false", "instance", inst.Name)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, upd := range updateResponse.Result {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}

			// Обрабатываем каждое обновление в отдельной горутине
			go handleTelegramUpdate(inst, upd)
		}
	}
}

// handleTelegramUpdate маршрутизирует входящие сообщения и нажатия кнопок.
func handleTelegramUpdate(inst *Instance, upd TelegramUpdate) {
	// 1. Проверяем текстовые сообщения (включая отредактированные)
	var msg *TelegramMessage
	isEdited := false
	if upd.Message != nil {
		msg = upd.Message
	} else if upd.EditedMessage != nil {
		msg = upd.EditedMessage
		isEdited = true
	}

	if msg != nil {
		chatIdStr := strconv.FormatInt(msg.Chat.ID, 10)
		if chatIdStr != inst.Telegram.ChatID {
			slog.Warn("Ignored Telegram message from unauthorized chat ID", "received", chatIdStr, "configured", inst.Telegram.ChatID, "is_edited", isEdited)
			return
		}

		text := strings.TrimSpace(msg.Text)
		if text == "" {
			return
		}

		// Вывод списка команд
		if text == "/menu" || text == "!menu" || text == "/nemu" || text == "!nemu" {
			// Проверяем дедупликацию по связке chat_id:message_id:menu
			if checkAndMarkTelegramExecuted(msg.Chat.ID, msg.MessageID, "menu") {
				slog.Debug("Skipping already executed Telegram menu command from edited message", "messageID", msg.MessageID)
				return
			}
			sendTelegramMenu(inst)
			return
		}

		// Запуск команды меню
		if strings.HasPrefix(text, "/") || strings.HasPrefix(text, "!") {
			targetMenu := findMenuByID(inst.Telegram.Menu)
			if targetMenu == nil {
				slog.Warn("Configured Menu ID not found", "menuID", inst.Telegram.Menu, "instance", inst.Name)
				return
			}

			matchedItem := findTelegramCommand(targetMenu, text)
			if matchedItem != nil {
				// Проверяем дедупликацию по связке chat_id:message_id:command_name
				if checkAndMarkTelegramExecuted(msg.Chat.ID, msg.MessageID, matchedItem.Name) {
					slog.Debug("Skipping already executed Telegram command from edited message", "messageID", msg.MessageID, "command", matchedItem.Name)
					return
				}
				go executeTelegramMenuCommand(inst, *matchedItem)
			}
		}
	}

	// 2. Проверяем нажатия Inline-кнопок
	if upd.CallbackQuery != nil {
		if upd.CallbackQuery.Message == nil {
			slog.Warn("Ignored Telegram callback with nil message")
			return
		}
		chatIdStr := strconv.FormatInt(upd.CallbackQuery.Message.Chat.ID, 10)
		if chatIdStr != inst.Telegram.ChatID {
			slog.Warn("Ignored Telegram callback from unauthorized chat ID", "received", chatIdStr, "configured", inst.Telegram.ChatID)
			return
		}

		// Сразу же гасим индикатор загрузки на кнопке
		answerUrl := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", inst.Telegram.BotToken)
		payload := map[string]string{
			"callback_query_id": upd.CallbackQuery.ID,
		}
		jsonData, _ := json.Marshal(payload)
		if resp, err := http.Post(answerUrl, "application/json", bytes.NewBuffer(jsonData)); err == nil {
			resp.Body.Close()
		}

		data := upd.CallbackQuery.Data
		if strings.HasPrefix(data, "cmd:") {
			cmdName := strings.TrimPrefix(data, "cmd:")
			targetMenu := findMenuByID(inst.Telegram.Menu)
			if targetMenu == nil {
				slog.Warn("Configured Menu ID not found", "menuID", inst.Telegram.Menu, "instance", inst.Name)
				return
			}

			var matchedItem *MenuItem
			for i := range targetMenu.Items {
				if targetMenu.Items[i].Name == cmdName {
					matchedItem = &targetMenu.Items[i]
					break
				}
			}

			if matchedItem != nil {
				go executeTelegramMenuCommand(inst, *matchedItem)
			}
		}
	}
}

// findTelegramCommand ищет команду в меню по её тексту. Поддерживает замену / на _ для совместимости.
func findTelegramCommand(targetMenu *Menu, text string) *MenuItem {
	if len(text) < 2 {
		return nil
	}
	// Отрезаем лидирующий префикс
	cleanCmd := text[1:]

	// 1. Пытаемся найти точное совпадение (например, "stats" или "snapshot/hd")
	for i := range targetMenu.Items {
		if targetMenu.Items[i].Name == cleanCmd {
			return &targetMenu.Items[i]
		}
	}

	// 2. Ищем с заменой слэшей на подчеркивания (например, если пользователь написал /snapshot_hd, а в меню snapshot/hd)
	for i := range targetMenu.Items {
		normalizedMenuName := strings.ReplaceAll(targetMenu.Items[i].Name, "/", "_")
		if normalizedMenuName == cleanCmd {
			return &targetMenu.Items[i]
		}
	}

	return nil
}

// sendTelegramMenu формирует и отправляет текстовый список команд с клавиатурой Inline-кнопок.
func sendTelegramMenu(inst *Instance) {
	targetMenu := findMenuByID(inst.Telegram.Menu)
	if targetMenu == nil {
		slog.Warn("Configured Menu ID not found for menu formatting", "menuID", inst.Telegram.Menu, "instance", inst.Name)
		return
	}

	var responseLines []string
	responseLines = append(responseLines, "📜 Доступные команды меню:")
	for _, item := range targetMenu.Items {
		cmdForDisplay := strings.ReplaceAll(item.Name, "/", "_")
		responseLines = append(responseLines, fmt.Sprintf("• /%s - %s", cmdForDisplay, item.Description))
	}
	responseText := strings.Join(responseLines, "\n")

	// Формируем клавиатуру по 2 кнопки в ряд
	var keyboard [][]InlineKeyboardButton
	var currentRow []InlineKeyboardButton
	for i, item := range targetMenu.Items {
		btnText := item.Name
		if item.Reaction != "" {
			btnText = fmt.Sprintf("%s %s", item.Reaction, item.Name)
		}
		btn := InlineKeyboardButton{
			Text:         btnText,
			CallbackData: "cmd:" + item.Name,
		}
		currentRow = append(currentRow, btn)
		if len(currentRow) == 2 || i == len(targetMenu.Items)-1 {
			keyboard = append(keyboard, currentRow)
			currentRow = nil
		}
	}

	markup := &InlineKeyboardMarkup{
		InlineKeyboard: keyboard,
	}

	err := sendTelegramMessageWithMarkup(inst.Telegram.BotToken, inst.Telegram.ChatID, responseText, markup, inst.Telegram.RetryCount, inst.Telegram.RetryDelay, "", "", "")
	if err != nil {
		slog.Error("Failed to send Telegram menu message", "instance", inst.Name, "error", err)
	}
}

// sendTelegramResponse отправляет мгновенный текстовый ответ в чат Telegram с маскированием учетных данных и раскодированием HTML сущностей.
func sendTelegramResponse(inst *Instance, text string) error {
	masked := maskCredentialsInText(text)
	unescaped := html.UnescapeString(masked)
	return sendTelegramMessage(inst.Telegram.BotToken, inst.Telegram.ChatID, unescaped, inst.Telegram.RetryCount, inst.Telegram.RetryDelay, "", "", "")
}

// executeTelegramMenuCommand выполняет локальный скрипт или URL команды и шлет отчет в Telegram.
func executeTelegramMenuCommand(inst *Instance, item MenuItem) {
	// 1. Если настроен локальный скрипт (имеет приоритет над URL)
	if item.Script != "" {
		slog.Info("Executing matched command script from Telegram", "command", item.Name, "script", item.Script)

		scriptPath := filepath.Join("/app/scripts", item.Script)

		// Проверяем существование файла скрипта
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			slog.Error("Script file does not exist", "path", scriptPath, "command", item.Name)
			_ = sendTelegramResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды /%s", strings.ReplaceAll(item.Name, "/", "_")))
			return
		}

		// Получаем таймаут выполнения
		timeoutSec := 15
		if envVal := os.Getenv("SCRIPT_TIMEOUT"); envVal != "" {
			if parsed, err := strconv.Atoi(envVal); err == nil && parsed > 0 {
				timeoutSec = parsed
			}
		}
		timeout := time.Duration(timeoutSec) * time.Second

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Запускаем скрипт через bash (установленный в Docker-образе), чтобы поддерживать
		// bash-специфичные конструкции и синтаксис (например, echo -e)
		cmd := exec.CommandContext(ctx, "bash", scriptPath)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err != nil {
			slog.Error("Failed to execute command script from Telegram", "command", item.Name, "script", item.Script, "error", err, "stderr", stderr.String())
			_ = sendTelegramResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды /%s", strings.ReplaceAll(item.Name, "/", "_")))
			return
		}

		outputBytes := stdout.Bytes()
		slog.Info("Command script executed successfully", "command", item.Name, "outputLen", len(outputBytes))

		if len(outputBytes) > 0 && strings.TrimSpace(string(outputBytes)) != "" {
			if isTextContent("", outputBytes) {
				// Пытаемся распарсить как JSON/XML
				if pairs, err := DecodeOrderedJSON(bytes.NewReader(outputBytes)); err == nil {
					formatted := formatOrderedJSONValue(pairs, 0)
					_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(formatted, 4000)))
				} else {
					var jsonVal interface{}
					if jsonErr := json.Unmarshal(outputBytes, &jsonVal); jsonErr == nil {
						switch concreteVal := jsonVal.(type) {
						case []interface{}:
							formatted := formatAnyJSONValue(concreteVal, 0)
							_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(formatted, 4000)))
						default:
							_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(string(outputBytes), 4000)))
						}
					} else if xmlVal, xmlErr := parseXML(outputBytes); xmlErr == nil {
						stripped := stripXMLRoot(xmlVal)
						formatted := formatJSONValue(stripped, 0)
						_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(formatted, 4000)))
					} else {
						_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(string(outputBytes), 4000)))
					}
				}
			} else {
				// Бинарный вывод
				tempDir := getMediaDir()
				_ = os.MkdirAll(tempDir, 0755)

				mimeType := http.DetectContentType(outputBytes)
				ext := ".bin"
				if strings.Contains(mimeType, "image/jpeg") {
					ext = ".jpg"
				} else if strings.Contains(mimeType, "image/png") {
					ext = ".png"
				} else if strings.Contains(mimeType, "image/gif") {
					ext = ".gif"
				}

				tempFileName := fmt.Sprintf("script_%s_%d%s", strings.ReplaceAll(item.Name, "/", "_"), time.Now().UnixNano(), ext)
				tempFilePath := filepath.Join(tempDir, tempFileName)

				if writeErr := os.WriteFile(tempFilePath, outputBytes, 0644); writeErr != nil {
					slog.Error("Failed to save script output binary to temp file for Telegram", "error", writeErr)
					_ = sendTelegramResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды /%s", strings.ReplaceAll(item.Name, "/", "_")))
					return
				}

				go func(filePath, fileName, mime string) {
					defer os.Remove(filePath)
					slog.Info("Sending script output binary as Telegram file", "fileName", fileName, "mime", mime)
					caption := fmt.Sprintf("✅ Результат команды /%s (бинарный вывод)", strings.ReplaceAll(item.Name, "/", "_"))
					if sendErr := sendTelegramMessage(inst.Telegram.BotToken, inst.Telegram.ChatID, caption, inst.Telegram.RetryCount, inst.Telegram.RetryDelay, filePath, fileName, mime); sendErr != nil {
						slog.Error("Failed to send script binary output to Telegram", "error", sendErr)
					}
				}(tempFilePath, tempFileName, mimeType)
			}
		} else {
			_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно (вывод пуст)", strings.ReplaceAll(item.Name, "/", "_")))
		}
		return
	}

	// 2. Если настроен URL
	if item.URL != "" {
		slog.Info("Executing matched command via HTTP GET from Telegram", "command", item.Name, "url", item.URL)

		httpClient := &http.Client{
			Timeout: 15 * time.Second,
		}

		resp, err := httpClient.Get(item.URL)
		if err != nil {
			slog.Error("Failed to execute command via HTTP GET from Telegram", "command", item.Name, "url", item.URL, "error", err)
			_ = sendTelegramResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды /%s", strings.ReplaceAll(item.Name, "/", "_")))
			return
		}
		defer resp.Body.Close()

		slog.Info("Command executed successfully from Telegram", "command", item.Name, "status", resp.Status)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			limitReader := io.LimitReader(resp.Body, 1024*1024)
			bodyBytes, readErr := io.ReadAll(limitReader)
			if readErr == nil && len(bodyBytes) > 0 && strings.TrimSpace(string(bodyBytes)) != "" && isTextContent(resp.Header.Get("Content-Type"), bodyBytes) {
				if pairs, err := DecodeOrderedJSON(bytes.NewReader(bodyBytes)); err == nil {
					formatted := formatOrderedJSONValue(pairs, 0)
					_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(formatted, 4000)))
				} else {
					var jsonVal interface{}
					if jsonErr := json.Unmarshal(bodyBytes, &jsonVal); jsonErr == nil {
						switch concreteVal := jsonVal.(type) {
						case []interface{}:
							formatted := formatAnyJSONValue(concreteVal, 0)
							_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(formatted, 4000)))
						default:
							_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(string(bodyBytes), 4000)))
						}
					} else if xmlVal, xmlErr := parseXML(bodyBytes); xmlErr == nil {
						stripped := stripXMLRoot(xmlVal)
						formatted := formatJSONValue(stripped, 0)
						_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(formatted, 4000)))
					} else {
						_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно:\n%s", strings.ReplaceAll(item.Name, "/", "_"), truncateText(string(bodyBytes), 4000)))
					}
				}
			} else {
				_ = sendTelegramResponse(inst, fmt.Sprintf("✅ Команда /%s выполнена успешно", strings.ReplaceAll(item.Name, "/", "_")))
			}
		} else {
			slog.Error("Command returned non-successful HTTP status from Telegram", "command", item.Name, "status", resp.Status)
			_ = sendTelegramResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды /%s", strings.ReplaceAll(item.Name, "/", "_")))
		}
		return
	}

	slog.Error("Command execution failed for Telegram: neither script nor url configured", "command", item.Name)
	_ = sendTelegramResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды /%s", strings.ReplaceAll(item.Name, "/", "_")))
}

func findMenuByID(menuID string) *Menu {
	configMu.RLock()
	defer configMu.RUnlock()
	for i := range globalConfig.Menus {
		if globalConfig.Menus[i].ID == menuID {
			m := globalConfig.Menus[i]
			// Создаем копию элементов меню для предотвращения Race Condition
			m.Items = append([]MenuItem(nil), globalConfig.Menus[i].Items...)
			return &m
		}
	}
	return nil
}