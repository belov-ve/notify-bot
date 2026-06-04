package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	_ "modernc.org/sqlite"
)

const AppVersion = "3.1.0"

// matrixClients и другие мапы теперь индексируются по "Account ID" (slug от username + homeserver)
var (
	matrixClients   = make(map[string]*mautrix.Client)
	cryptoHelpers   = make(map[string]*cryptohelper.CryptoHelper)
	matrixDBs       = make(map[string]*sql.DB)
	matrixSyncReady = make(map[string]chan struct{})
	matrixMu        sync.Mutex

	roomMembersCache = make(map[id.RoomID][]id.UserID)
	roomMembersMu    sync.Mutex

	sessionResetCache = make(map[id.RoomID]bool)
	sessionResetMu    sync.Mutex

	// Mutexes for serializing writes per account (single write point)
	accountLocks   = make(map[string]*sync.Mutex)
	accountLocksMu sync.Mutex
)

// getAccountLock returns a personal mutex for a specific Matrix account
func getAccountLock(accountID string) *sync.Mutex {
	accountLocksMu.Lock()
	defer accountLocksMu.Unlock()
	lock, ok := accountLocks[accountID]
	if !ok {
		lock = &sync.Mutex{}
		accountLocks[accountID] = lock
	}
	return lock
}

// getAccountID создает лаконичный и уникальный ID на основе Matrix ID пользователя.
func getAccountID(username, homeserver string) string {
	return strings.ReplaceAll(strings.TrimPrefix(username, "@"), ":", "_")
}

// verificationHandler реализует интерфейсы верификации.
type verificationHandler struct {
	vh        *verificationhelper.VerificationHelper
	accountID string
}

func (h *verificationHandler) VerificationRequested(ctx context.Context, txnID id.VerificationTransactionID, from id.UserID, fromDevice id.DeviceID) {
	slog.Info("Matrix verification requested", "account", h.accountID, "from", from, "device", fromDevice)
	_ = h.vh.AcceptVerification(ctx, txnID)
	time.AfterFunc(1*time.Second, func() {
		_ = h.vh.StartSAS(context.Background(), txnID)
	})
}

func (h *verificationHandler) VerificationCancelled(ctx context.Context, txnID id.VerificationTransactionID, code event.VerificationCancelCode, reason string) {
	slog.Warn("Matrix verification cancelled", "account", h.accountID, "reason", reason)
}

func (h *verificationHandler) VerificationDone(ctx context.Context, txnID id.VerificationTransactionID) {
	slog.Info("Matrix verification completed successfully!", "account", h.accountID)
}

func (h *verificationHandler) ShowSAS(ctx context.Context, txnID id.VerificationTransactionID, emojis []rune, decimals []int) {
	slog.Info("--- MATRIX VERIFICATION EMOJIS ---", "account", h.accountID)
	if len(emojis) > 0 {
		slog.Info("Compare emojis:", "emojis", string(emojis))
	}
	if len(decimals) > 0 {
		slog.Info("Compare numbers:", "numbers", decimals)
	}
	time.AfterFunc(3*time.Second, func() {
		slog.Info("Confirming SAS...", "account", h.accountID)
		_ = h.vh.ConfirmSAS(context.Background(), txnID)
	})
}

func saveSyncToken(accountID string, db *sql.DB, token string) {
	_, err := db.Exec("INSERT OR REPLACE INTO local_metadata (key, value) VALUES ('sync_token', ?)", token)
	if err != nil {
		slog.Warn("Failed to save sync token", "account", accountID, "error", err)
	}
}

func loadSyncToken(db *sql.DB) string {
	var token string
	_ = db.QueryRow("SELECT value FROM local_metadata WHERE key = 'sync_token'").Scan(&token)
	return token
}

func saveSessionInfo(db *sql.DB, userID, token string) {
	_, _ = db.Exec("INSERT OR REPLACE INTO local_metadata (key, value) VALUES ('access_token', ?)", token)
	_, _ = db.Exec("INSERT OR REPLACE INTO local_metadata (key, value) VALUES ('user_id', ?)", userID)
}

func loadSessionInfo(db *sql.DB) (string, string) {
	var userID, token string
	_ = db.QueryRow("SELECT value FROM local_metadata WHERE key = 'user_id'").Scan(&userID)
	_ = db.QueryRow("SELECT value FROM local_metadata WHERE key = 'access_token'").Scan(&token)
	return userID, token
}

func getOrCreateDeviceID(accountID string, db *sql.DB) id.DeviceID {
	var deviceID string
	err := db.QueryRow("SELECT value FROM local_metadata WHERE key = 'device_id'").Scan(&deviceID)
	if err == nil && deviceID != "" {
		return id.DeviceID(deviceID)
	}

	b := make([]byte, 4)
	_, _ = rand.Read(b)
	suffix := hex.EncodeToString(b)
	newID := fmt.Sprintf("notify-bot-%s", suffix)

	_, err = db.Exec("INSERT OR REPLACE INTO local_metadata (key, value) VALUES ('device_id', ?)", newID)
	if err != nil {
		slog.Warn("Failed to save DeviceID", "account", accountID, "error", err)
	}
	return id.DeviceID(newID)
}

func ResetMatrixClient(accountID string) {
	matrixMu.Lock()
	defer matrixMu.Unlock()

	if db, ok := matrixDBs[accountID]; ok {
		_ = db.Close()
	}
	delete(matrixClients, accountID)
	delete(cryptoHelpers, accountID)
	delete(matrixDBs, accountID)
	delete(matrixSyncReady, accountID)
}

func getMatrixClient(inst *Instance) (*mautrix.Client, error) {
	conf := inst.Matrix
	accountID := getAccountID(conf.Username, conf.Homeserver)

	matrixMu.Lock()
	if client, ok := matrixClients[accountID]; ok {
		matrixMu.Unlock()
		return client, nil
	}

	client, syncReadyChan, err := createMatrixClient(inst, accountID)
	matrixMu.Unlock()

	if err != nil {
		return nil, err
	}

	if conf.Encryption && syncReadyChan != nil {
		slog.Info("Waiting for first Matrix sync to upload keys...", "account", accountID)
		select {
		case <-syncReadyChan:
			slog.Info("Matrix client is fully ready after first sync", "account", accountID)
		case <-time.After(15 * time.Second):
			slog.Warn("Timed out waiting for first Matrix sync, proceeding with sending", "account", accountID)
		}
	}

	return client, nil
}

func createMatrixClient(inst *Instance, accountID string) (*mautrix.Client, chan struct{}, error) {
	conf := inst.Matrix
	client, err := mautrix.NewClient(conf.Homeserver, "", "")
	if err != nil {
		return nil, nil, err
	}

	dbDir := "/app/data"
	if os.Getenv("DB_PATH") != "" {
		dbDir = filepath.Dir(os.Getenv("DB_PATH"))
	}
	cryptoDBPath := filepath.Join(dbDir, fmt.Sprintf("%s.db", accountID))

	// Используем URI DSN для автоматического применения PRAGMA ко всем соединениям в пуле.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)", cryptoDBPath)
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}

	// Снижаем лимиты пула соединений до безопасного минимума (2 соединения для предотвращения взаимных блокировок).
	// Наш прикладной мьютекс accountLock гарантирует единую точку записи и отсутствие гонок в рамках Go-процесса.
	rawDB.SetMaxOpenConns(2)
	rawDB.SetMaxIdleConns(1)

	_, _ = rawDB.Exec("CREATE TABLE IF NOT EXISTS local_metadata (key TEXT PRIMARY KEY, value TEXT)")
	matrixDBs[accountID] = rawDB

	deviceID := getOrCreateDeviceID(accountID, rawDB)
	userID, accessToken := loadSessionInfo(rawDB)

	deviceDisplayName := "Notify-Bot"

	if accessToken != "" && userID != "" {
		client.AccessToken = accessToken
		client.UserID = id.UserID(userID)
		client.DeviceID = deviceID
	} else if conf.AccessToken != "" {
		client.AccessToken = conf.AccessToken
		client.UserID = id.UserID(conf.Username)
		client.DeviceID = deviceID
	} else if conf.Password != "" {
		slog.Info("Logging in to Matrix...", "account", accountID)
		resp, err := client.Login(context.Background(), &mautrix.ReqLogin{
			Type: mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{
				Type: mautrix.IdentifierTypeUser,
				User: conf.Username,
			},
			Password:                 conf.Password,
			DeviceID:                 deviceID,
			InitialDeviceDisplayName: deviceDisplayName,
		})
		if err != nil {
			_ = rawDB.Close()
			return nil, nil, err
		}
		client.AccessToken = resp.AccessToken
		client.UserID = resp.UserID
		client.DeviceID = resp.DeviceID
		saveSessionInfo(rawDB, string(client.UserID), client.AccessToken)
	} else {
		_ = rawDB.Close()
		return nil, nil, fmt.Errorf("no credentials for Matrix account %s", accountID)
	}

	_ = client.SetDeviceInfo(context.Background(), client.DeviceID, &mautrix.ReqDeviceInfo{DisplayName: deviceDisplayName})

	var syncReadyChan chan struct{}

	if conf.Encryption || conf.Menu != "" {
		var helper *cryptohelper.CryptoHelper

		if conf.Encryption {
			slog.Info("Initializing Matrix E2EE", "account", accountID)

			cryptoDB, err := dbutil.NewWithDB(rawDB, "sqlite")
			if err != nil {
				_ = rawDB.Close()
				return nil, nil, err
			}

			helper, err = cryptohelper.NewCryptoHelper(client, []byte("pickle-"+accountID), cryptoDB)
			if err != nil {
				_ = rawDB.Close()
				return nil, nil, err
			}

			if conf.Password != "" {
				helper.LoginAs = &mautrix.ReqLogin{
					Type: mautrix.AuthTypePassword,
					Identifier: mautrix.UserIdentifier{
						Type: mautrix.IdentifierTypeUser,
						User: string(client.UserID),
					},
					Password: conf.Password,
					DeviceID: client.DeviceID,
				}
			}

			err = helper.Init(context.Background())
			if err != nil {
				_ = rawDB.Close()
				return nil, nil, err
			}

			_ = helper.Machine().ShareKeys(context.Background(), 0)
			helper.Machine().ShareKeysMinTrust = id.TrustStateUnset

			helper.Machine().AllowKeyShare = func(ctx context.Context, device *id.Device, info event.RequestedKeyInfo) *crypto.KeyShareRejection {
				if device == nil {
					slog.Warn("Key share requested by untracked/nil device", "roomID", info.RoomID, "sessionID", info.SessionID)
					return nil
				}
				slog.Info("Key share requested by device", "user", device.UserID, "device", device.DeviceID, "sessionID", info.SessionID)
				return nil
			}

			fingerprint := helper.Machine().OwnIdentity().Fingerprint()
			var formattedFingerprint string
			for i, r := range fingerprint {
				if i > 0 && i%4 == 0 {
					formattedFingerprint += " "
				}
				formattedFingerprint += string(r)
			}
			slog.Info("Matrix device identity", "account", accountID, "device_id", client.DeviceID, "fingerprint", formattedFingerprint)

			client.Crypto = helper
			cryptoHelpers[accountID] = helper
		}

		syncer := mautrix.NewDefaultSyncer()
		client.Syncer = syncer

		if conf.Encryption {
			vhStore := verificationhelper.NewInMemoryVerificationStore()
			handler := &verificationHandler{accountID: accountID}
			vh := verificationhelper.NewVerificationHelper(client, helper.Machine(), vhStore, handler, false)
			handler.vh = vh
			_ = vh.Init(context.Background())

			syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
				roomMembersMu.Lock()
				delete(roomMembersCache, evt.RoomID)
				roomMembersMu.Unlock()
				slog.Info("Matrix room membership changed, member cache invalidated",
					"roomID", evt.RoomID,
					"userID", evt.GetStateKey(),
					"account", accountID)
			})
		}

		if conf.Menu != "" {
			slog.Info("Registering Matrix message and reaction handlers for menu", "account", accountID, "menuID", conf.Menu)

			// 1. Обработка обычных (открытых) сообщений и реакций
			syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
				handleMatrixMessage(client, inst, evt)
			})
			syncer.OnEventType(event.EventReaction, func(ctx context.Context, evt *event.Event) {
				handleMatrixReaction(client, inst, evt)
			})

			// 2. Обработка зашифрованных сообщений (E2EE)
			if conf.Encryption {
				slog.Info("Registering Matrix E2EE message and reaction handlers for menu", "account", accountID)
				syncer.OnEventType(event.EventEncrypted, func(ctx context.Context, evt *event.Event) {
					// Игнорируем зашифрованные события из истории (более 15 секунд назад), чтобы не логировать ошибки расшифровки
					if evt.Timestamp > 0 && time.Since(time.UnixMilli(evt.Timestamp)) > 15*time.Second {
						slog.Debug("Skipping historical encrypted Matrix event to prevent decryption warnings", "eventID", evt.ID)
						return
					}
					helper := cryptoHelpers[accountID]
					if helper != nil {
						decrypted, err := helper.Decrypt(ctx, evt)
						if err != nil {
							slog.Error("Failed to decrypt incoming Matrix event", "eventID", evt.ID, "error", err)
							return
						}
						slog.Debug("Decrypted incoming Matrix event successfully", "eventID", decrypted.ID, "type", decrypted.Type)
						if decrypted.Type == event.EventMessage {
							handleMatrixMessage(client, inst, decrypted)
						} else if decrypted.Type == event.EventReaction {
							handleMatrixReaction(client, inst, decrypted)
						}
					}
				})
			}
		}

		syncReadyChan = make(chan struct{})
		matrixSyncReady[accountID] = syncReadyChan

		go func(c *mautrix.Client, accID string, db *sql.DB, readyChan chan struct{}) {
			slog.Debug("Starting Matrix sync loop", "account", accID)

			// Load sync token under mutex (write queue serialization)
			lock := getAccountLock(accID)
			lock.Lock()
			since := loadSyncToken(db)
			lock.Unlock()

			isFirstSync := true
			for {
				matrixMu.Lock()
				currentClient := matrixClients[accID]
				matrixMu.Unlock()
				if currentClient != c {
					slog.Info("Matrix client reset detected, terminating obsolete sync loop", "account", accID)
					return
				}

				resp, err := c.SyncRequest(context.Background(), 30, since, "", false, event.PresenceOnline)
				if err != nil {
					if merr, ok := err.(mautrix.HTTPError); ok && (merr.IsStatus(401) || merr.IsStatus(403)) {
						// Clear tokens under mutex
						lock.Lock()
						_, _ = db.Exec("DELETE FROM local_metadata WHERE key IN ('access_token', 'user_id')")
						lock.Unlock()
						ResetMatrixClient(accID)
						return
					}
					time.Sleep(10 * time.Second)
					continue
				}

				matrixMu.Lock()
				currentClient = matrixClients[accID]
				matrixMu.Unlock()
				if currentClient != c {
					slog.Info("Matrix client reset detected after sync request, terminating sync loop", "account", accID)
					return
				}

				// Process sync responses and save token under mutex (single write point)
				lock.Lock()
				if conf.Encryption {
					helper := cryptoHelpers[accID]
					if helper != nil {
						helper.Machine().ProcessSyncResponse(context.Background(), resp, since)
					}
				}
				c.Syncer.ProcessResponse(context.Background(), resp, since)
				since = resp.NextBatch
				saveSyncToken(accID, db, since)
				lock.Unlock()

				if isFirstSync {
					isFirstSync = false
					close(readyChan)
					slog.Info("First Matrix sync completed", "account", accID)
				}
			}
		}(client, accountID, rawDB, syncReadyChan)

		if conf.Encryption {
			slog.Info("Matrix E2EE initialized successfully", "account", accountID)
		} else {
			slog.Info("Matrix listener initialized successfully (no E2EE)", "account", accountID)
		}
	}

	matrixClients[accountID] = client
	return client, syncReadyChan, nil
}

// handleMatrixMessage обрабатывает входящие текстовые сообщения из Matrix.
// Поддерживает команду !menu для вывода списка доступных действий и
// запуск команд по шаблону !<команда> в асинхронном режиме.
func handleMatrixMessage(client *mautrix.Client, inst *Instance, evt *event.Event) {
	conf := inst.Matrix
	if conf == nil {
		return
	}

	// Логируем каждое входящее событие для диагностики
	slog.Debug("Matrix message handler triggered",
		"eventType", evt.Type,
		"roomID", evt.RoomID,
		"configuredRoomID", conf.RoomID,
		"sender", evt.Sender,
		"botUserID", client.UserID,
	)

	// 1. Игнорируем сообщения от самого бота во избежание бесконечных циклов отправки.
	if evt.Sender == client.UserID {
		return
	}

	// Игнорируем старые сообщения из истории (более 15 секунд назад), например, при стартовом sync.
	if evt.Timestamp > 0 && time.Since(time.UnixMilli(evt.Timestamp)) > 15*time.Second {
		slog.Debug("Skipping historical Matrix message", "eventID", evt.ID, "age", time.Since(time.UnixMilli(evt.Timestamp)))
		return
	}

	if conf.Menu == "" {
		return
	}

	// 2. Реагируем только на сообщения в целевой (настроенной) комнате инстанса.
	if string(evt.RoomID) != conf.RoomID {
		slog.Debug("Matrix event skipped: Room ID mismatch", "roomID", evt.RoomID, "configuredRoomID", conf.RoomID)
		return
	}

	// 3. Извлекаем текстовое содержимое сообщения.
	_ = evt.Content.ParseRaw(evt.Type)

	slog.Debug("Matrix event parsed content type", "type", fmt.Sprintf("%T", evt.Content.Parsed))

	msgContent, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		slog.Debug("Matrix event skipped: not MessageEventContent", "type", fmt.Sprintf("%T", evt.Content.Parsed))
		return
	}

	// Нас интересуют только текстовые сообщения.
	if msgContent.MsgType != event.MsgText {
		return
	}

	body := strings.TrimSpace(msgContent.Body)
	if body == "" {
		return
	}

	// 4. Поиск настроенного меню в глобальной конфигурации.
	if globalConfig == nil {
		slog.Warn("Global configuration is nil, skipping Matrix command processing", "roomID", evt.RoomID)
		return
	}

	menuID := conf.Menu
	if menuID == "" {
		return
	}

	var targetMenu *Menu
	for i := range globalConfig.Menus {
		if globalConfig.Menus[i].ID == menuID {
			targetMenu = &globalConfig.Menus[i]
			break
		}
	}

	if targetMenu == nil {
		slog.Warn("Configured Menu ID not found in global configuration", "menuID", menuID, "instance", inst.Name)
		return
	}

	// 5. Обработка команды вывода меню (!menu или !nemu для отказоустойчивости к опечаткам).
	if body == "!menu" || body == "!nemu" {
		slog.Info("Command !menu received, formatting menu response", "account", getAccountID(conf.Username, conf.Homeserver), "roomID", evt.RoomID)

		// Формируем текстовое меню
		var responseLines []string
		responseLines = append(responseLines, "Доступные команды:")
		for _, item := range targetMenu.Items {
			reactionHint := ""
			if item.Reaction != "" {
				reactionHint = fmt.Sprintf(" (%s)", item.Reaction)
			}
			responseLines = append(responseLines, fmt.Sprintf("• !%s%s - %s", item.Name, reactionHint, item.Description))
		}
		replyText := strings.Join(responseLines, "\n")

		// Отправляем ответ асинхронно в отдельной горутине, чтобы избежать взаимной блокировки (Deadlock)
		// с циклом синхронизации Matrix, который держит accountLock.
		go func() {
			if err := sendMatrixResponse(inst, replyText); err != nil {
				slog.Error("Failed to send menu reply to Matrix", "error", err)
			}
		}()
		return
	}

	// 6. Обработка пользовательских команд из меню.
	if strings.HasPrefix(body, "!") {
		cmdName := strings.TrimPrefix(body, "!")
		var matchedItem *MenuItem
		for i := range targetMenu.Items {
			if targetMenu.Items[i].Name == cmdName {
				matchedItem = &targetMenu.Items[i]
				break
			}
		}

		// Если команда найдена в списке меню инстанса
		if matchedItem != nil {
			slog.Info("Executing matched command from Matrix chat", "command", cmdName, "url", matchedItem.URL)

			// Запускаем HTTP GET запрос асинхронно, чтобы не блокировать поток обработки сообщений Matrix.
			go func(item MenuItem) {
				httpClient := &http.Client{
					Timeout: 15 * time.Second,
				}

				resp, err := httpClient.Get(item.URL)
				if err != nil {
					slog.Error("Failed to execute command via HTTP GET", "command", item.Name, "url", item.URL, "error", err)
					_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s: %v", item.Name, err))
					return
				}
				defer resp.Body.Close()

				slog.Info("Command executed successfully", "command", item.Name, "status", resp.Status)
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно", item.Name))
				} else {
					_ = sendMatrixResponse(inst, fmt.Sprintf("⚠️ Команда !%s вернула статус: %s", item.Name, resp.Status))
				}
			}(*matchedItem)
		}
	}
}

// handleMatrixReaction обрабатывает входящие эмодзи-реакции из Matrix (m.reaction).
// Если реакция соответствует настроенной в меню команде, бот выполняет ее асинхронно.
func handleMatrixReaction(client *mautrix.Client, inst *Instance, evt *event.Event) {
	conf := inst.Matrix
	if conf == nil {
		return
	}

	// Логируем входящую реакцию для диагностики
	slog.Debug("Matrix reaction handler triggered",
		"eventType", evt.Type,
		"roomID", evt.RoomID,
		"configuredRoomID", conf.RoomID,
		"sender", evt.Sender,
		"botUserID", client.UserID,
	)

	// 1. Игнорируем реакции от самого бота.
	if evt.Sender == client.UserID {
		return
	}

	// Игнорируем старые реакции из истории (более 15 секунд назад).
	if evt.Timestamp > 0 && time.Since(time.UnixMilli(evt.Timestamp)) > 15*time.Second {
		slog.Debug("Skipping historical Matrix reaction", "eventID", evt.ID, "age", time.Since(time.UnixMilli(evt.Timestamp)))
		return
	}

	if conf.Menu == "" {
		return
	}

	// 2. Реагируем только на события в целевой (настроенной) комнате инстанса.
	if string(evt.RoomID) != conf.RoomID {
		slog.Debug("Matrix reaction skipped: Room ID mismatch", "roomID", evt.RoomID, "configuredRoomID", conf.RoomID)
		return
	}

	// 3. Распарсиваем контент реакции.
	_ = evt.Content.ParseRaw(evt.Type)
	reactionContent, ok := evt.Content.Parsed.(*event.ReactionEventContent)
	if !ok {
		slog.Debug("Matrix reaction skipped: not ReactionEventContent", "type", fmt.Sprintf("%T", evt.Content.Parsed))
		return
	}

	emoji := strings.TrimSpace(reactionContent.RelatesTo.Key)
	if emoji == "" {
		return
	}

	// 4. Логируем полученную реакцию и её Unicode коды на уровне DEBUG для диагностики
	var runeList []string
	for _, r := range emoji {
		runeList = append(runeList, fmt.Sprintf("%U", r))
	}
	slog.Debug("Matrix reaction received",
		"emoji", emoji,
		"runes", strings.Join(runeList, " "),
		"ageSeconds", time.Since(time.UnixMilli(evt.Timestamp)).Seconds(),
	)

	// 5. Поиск настроенного меню в глобальной конфигурации.
	if globalConfig == nil {
		slog.Warn("Global configuration is nil, skipping Matrix reaction processing", "roomID", evt.RoomID)
		return
	}

	var targetMenu *Menu
	for i := range globalConfig.Menus {
		if globalConfig.Menus[i].ID == conf.Menu {
			targetMenu = &globalConfig.Menus[i]
			break
		}
	}

	if targetMenu == nil {
		slog.Warn("Configured Menu ID not found in global configuration", "menuID", conf.Menu, "instance", inst.Name)
		return
	}

	// 6. Поиск команды по совпадению эмодзи.
	cleanEmoji := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.ReplaceAll(s, "\ufe0f", "")
		s = strings.ReplaceAll(s, "\ufe0e", "")
		return s
	}

	var matchedItem *MenuItem
	for i := range targetMenu.Items {
		cleanedTarget := cleanEmoji(targetMenu.Items[i].Reaction)
		cleanedIncoming := cleanEmoji(emoji)

		var targetRunes []string
		for _, r := range targetMenu.Items[i].Reaction {
			targetRunes = append(targetRunes, fmt.Sprintf("%U", r))
		}

		slog.Debug("Comparing reaction to menu item",
			"menuItemName", targetMenu.Items[i].Name,
			"targetReaction", targetMenu.Items[i].Reaction,
			"targetRunes", strings.Join(targetRunes, " "),
			"cleanedTarget", cleanedTarget,
			"cleanedIncoming", cleanedIncoming,
			"match", cleanedTarget == cleanedIncoming,
		)

		if targetMenu.Items[i].Reaction != "" && cleanedTarget == cleanedIncoming {
			matchedItem = &targetMenu.Items[i]
			break
		}
	}

	// Если команда найдена по эмодзи
	if matchedItem != nil {
		slog.Info("Executing matched command from Matrix reaction", "command", matchedItem.Name, "emoji", emoji, "url", matchedItem.URL)

		// Запускаем HTTP GET запрос асинхронно в горутине, чтобы избежать взаимной блокировки (Deadlock)
		// с циклом синхронизации Matrix, который держит accountLock.
		go func(item MenuItem) {
			httpClient := &http.Client{
				Timeout: 15 * time.Second,
			}

			resp, err := httpClient.Get(item.URL)
			if err != nil {
				slog.Error("HTTP GET for reaction command failed", "command", item.Name, "url", item.URL, "error", err)
				_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s по реакции: %v", item.Name, err))
				return
			}
			defer resp.Body.Close()
			slog.Debug("HTTP GET response received for reaction command", "command", item.Name, "status", resp.Status)

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				slog.Info("Command executed successfully via reaction", "command", item.Name, "status", resp.Status)
				_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно по реакции", item.Name))
			} else {
				slog.Warn("Command returned non‑successful HTTP status via reaction", "command", item.Name, "status", resp.Status)
				_ = sendMatrixResponse(inst, fmt.Sprintf("⚠️ Команда !%s по реакции вернула статус: %s", item.Name, resp.Status))
			}
		}(*matchedItem)
	} else {
		// Команда по эмодзи не найдена
		slog.Debug("Matrix reaction: no matching command for emoji", "emoji", emoji)
	}
}

// sendMatrixResponse отправляет мгновенное текстовое сообщение в комнату Matrix в обход очереди.
func sendMatrixResponse(inst *Instance, text string) error {
	return sendMatrixWithRetry(inst, text, "", "", "")
}

// InitializeSyncClients принудительно инициализирует клиентов Matrix, у которых
// настроено интерактивное меню (matrix.menu != "").
// Это позволяет боту реагировать на сообщения сразу после старта программы.
func InitializeSyncClients(cfg *Config) {
	for i := range cfg.Instances {
		inst := &cfg.Instances[i]
		if inst.Enabled && inst.Matrix != nil && inst.Matrix.Enabled && inst.Matrix.Menu != "" {
			slog.Info("Pre-initializing Matrix sync client", "instance", inst.Name)
			go func(instance *Instance) {
				_, err := getMatrixClient(instance)
				if err != nil {
					slog.Error("Failed to pre-initialize Matrix sync client on startup", "instance", instance.Name, "error", err)
				}
			}(inst)
		}
	}
}

// sendMatrixWithRetry отправляет текстовое сообщение или файл в Matrix-комнату.
// Поддерживает загрузку файлов/картинок на homeserver и автоматическое шифрование E2EE.
func sendMatrixWithRetry(inst *Instance, text string, filePath, fileName, mimeType string) error {
	conf := inst.Matrix
	accountID := getAccountID(conf.Username, conf.Homeserver)
	retryCount := conf.RetryCount
	if retryCount <= 0 {
		retryCount = 3
	}
	retryDelay := conf.RetryDelay
	if retryDelay <= 0 {
		retryDelay = 2
	}

	delay := time.Duration(retryDelay) * time.Second

	for attempt := 1; attempt <= retryCount; attempt++ {
		client, err := getMatrixClient(inst)
		if err != nil {
			slog.Error("Failed to get Matrix client", "account", accountID, "error", err)
			time.Sleep(delay)
			delay *= 2
			continue
		}

		roomID := id.RoomID(conf.RoomID)

		// Upload file to server if provided
		var mxcURL string
		var encryptedFileInfo *event.EncryptedFileInfo
		var fileSize int64

		if filePath != "" {
			fileBytes, readErr := os.ReadFile(filePath)
			if readErr != nil {
				slog.Error("Failed to read attachment file for Matrix", "path", filePath, "error", readErr)
				err = readErr
			} else {
				fileSize = int64(len(fileBytes))
				uploadMime := mimeType
				if uploadMime == "" {
					uploadMime = "application/octet-stream"
				}

				if conf.Encryption {
					// В зашифрованной комнате: шифруем файл перед отправкой на сервер
					ef := attachment.NewEncryptedFile()
					ciphertext := ef.Encrypt(fileBytes)

					slog.Debug("Uploading encrypted attachment to homeserver", "name", fileName, "size", fileSize, "account", accountID)
					resp, uploadErr := client.UploadMedia(context.Background(), mautrix.ReqUploadMedia{
						Content:       bytes.NewReader(ciphertext),
						ContentLength: int64(len(ciphertext)),
						ContentType:   "application/octet-stream",
						FileName:      fileName,
					})
					if uploadErr != nil {
						slog.Error("Failed to upload encrypted file to homeserver", "error", uploadErr)
						err = uploadErr
					} else {
						mxcURL = resp.ContentURI.String()
						encryptedFileInfo = &event.EncryptedFileInfo{
							EncryptedFile: *ef,
							URL:           id.ContentURIString(mxcURL),
						}
					}
				} else {
					// В незашифрованной комнате: загружаем как есть
					slog.Debug("Uploading plaintext attachment to homeserver", "name", fileName, "size", fileSize, "account", accountID)
					resp, uploadErr := client.UploadMedia(context.Background(), mautrix.ReqUploadMedia{
						Content:       bytes.NewReader(fileBytes),
						ContentLength: fileSize,
						ContentType:   uploadMime,
						FileName:      fileName,
					})
					if uploadErr != nil {
						slog.Error("Failed to upload file to homeserver", "error", uploadErr)
						err = uploadErr
					} else {
						mxcURL = resp.ContentURI.String()
					}
				}
			}
		}

		if err != nil {
			slog.Warn("Failed to prepare attachment for Matrix", "attempt", attempt, "error", err)
			if attempt < retryCount {
				time.Sleep(delay)
				delay *= 2
				continue
			}
			return fmt.Errorf("failed to prepare Matrix attachment: %w", err)
		}

		// Формируем контент события
		var content *event.MessageEventContent
		if mxcURL != "" || encryptedFileInfo != nil {
			// Выбираем тип: m.image для картинок, m.file для документов
			msgtype := event.MsgFile
			if strings.HasPrefix(mimeType, "image/") {
				msgtype = event.MsgImage
			}
			content = &event.MessageEventContent{
				MsgType: msgtype,
				Body:    fileName,
				Info: &event.FileInfo{
					MimeType: mimeType,
					Size:     int(fileSize),
				},
			}
			if conf.Encryption && encryptedFileInfo != nil {
				content.File = encryptedFileInfo
			} else {
				content.URL = id.ContentURIString(mxcURL)
			}
		} else {
			// Обычное текстовое сообщение
			content = &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    text,
			}
		}

		var resp *mautrix.RespSendEvent
		if conf.Encryption {
			matrixMu.Lock()
			helper := cryptoHelpers[accountID]
			matrixMu.Unlock()

			if helper != nil {
				roomMembersMu.Lock()
				userIDs, found := roomMembersCache[roomID]
				roomMembersMu.Unlock()

				if !found {
					slog.Info("Fetching room members for E2EE", "roomID", roomID, "account", accountID)
					members, mErr := client.JoinedMembers(context.Background(), roomID)
					if mErr == nil {
						userIDs = make([]id.UserID, 0, len(members.Joined))
						for userID := range members.Joined {
							userIDs = append(userIDs, userID)
						}
						roomMembersMu.Lock()
						roomMembersCache[roomID] = userIDs
						roomMembersMu.Unlock()
						slog.Debug("Room members cached", "roomID", roomID, "count", len(userIDs))
					} else {
						slog.Error("Failed to fetch room members", "roomID", roomID, "error", mErr)
						err = mErr
					}
				}

				// Захватываем мьютекс для сериализации E2EE-операций в БД (очередь записи)
				lock := getAccountLock(accountID)
				lock.Lock()

				if err == nil && len(userIDs) > 0 {
					// Загружаем ключи устройств в локальную БД OlmMachine
					_, qErr := helper.Machine().FetchKeys(context.Background(), userIDs, true)
					if qErr != nil {
						slog.Warn("Failed to fetch device keys for room members", "roomID", roomID, "error", qErr)
						err = qErr
					}
				}

				if err == nil && len(userIDs) > 0 {
					// Автоматический сброс сессии (Self-healing) при первом сообщении после старта бота
					sessionResetMu.Lock()
					resetDone := sessionResetCache[roomID]
					if !resetDone {
						sessionResetCache[roomID] = true
					}
					sessionResetMu.Unlock()

					if !resetDone {
						slog.Info("Performing initial session reset for room to guarantee clean E2EE state", "roomID", roomID)
						_ = helper.Machine().CryptoStore.RemoveOutboundGroupSession(context.Background(), roomID)
					}

					slog.Debug("Sharing group session with members", "roomID", roomID, "count", len(userIDs))
					err = helper.Machine().ShareGroupSession(context.Background(), roomID, userIDs)
					if err != nil && strings.Contains(err.Error(), "group session already shared") {
						err = nil
					}
				}

				var encryptedContent *event.EncryptedEventContent
				if err == nil {
					encryptedContent, err = helper.Encrypt(context.Background(), roomID, event.EventMessage, content)
				}

				// Освобождаем мьютекс перед сетевым вызовом, так как шифрование и работа с БД завершены
				lock.Unlock()

				if err == nil {
					resp, err = client.SendMessageEvent(context.Background(), roomID, event.EventEncrypted, encryptedContent)
				}
			} else {
				err = fmt.Errorf("crypto helper not found for %s", accountID)
			}
		} else {
			resp, err = client.SendMessageEvent(context.Background(), roomID, event.EventMessage, content)
		}

		if err == nil && resp != nil {
			slog.Info("Matrix message sent", "account", accountID, "eventID", resp.EventID)

			// Если файл успешно отправлен и есть текстовый комментарий, отправляем его вторым сообщением
			if (mxcURL != "" || encryptedFileInfo != nil) && text != "" {
				slog.Debug("Sending accompanying text description for Matrix file", "account", accountID)
				if extraErr := sendMatrixWithRetry(inst, text, "", "", ""); extraErr != nil {
					slog.Warn("Failed to send accompanying text for Matrix", "error", extraErr)
				}
			}
			return nil
		}

		slog.Warn("Matrix send failed", "account", accountID, "attempt", attempt, "error", err)
		if merr, ok := err.(mautrix.HTTPError); ok && (merr.IsStatus(401) || merr.IsStatus(403)) {
			ResetMatrixClient(accountID)
		}

		if attempt < retryCount {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return fmt.Errorf("failed to send Matrix message after %d attempts", retryCount)
}
