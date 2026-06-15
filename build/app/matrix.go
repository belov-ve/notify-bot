package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"path/filepath"
	"sort"
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

const AppVersion = "3.2.2"

// matrixClients и другие мапы теперь индексируются по "Account ID" (slug от username + homeserver)
var (
	matrixClients   = make(map[string]*mautrix.Client)
	cryptoHelpers   = make(map[string]*cryptohelper.CryptoHelper)
	matrixDBs       = make(map[string]*sql.DB)
	matrixSyncReady = make(map[string]chan struct{})
	matrixMu        sync.Mutex


	// Mutexes for serializing writes per account (single write point)
	accountLocks   = make(map[string]*sync.Mutex)
	accountLocksMu sync.Mutex
)

type cachedMembers struct {
	userIDs  []id.UserID
	cachedAt time.Time
}

var (
	roomMembersCache = make(map[id.RoomID]cachedMembers)
	roomMembersMu    sync.Mutex
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
				// Если ключи шифрования Olm были сброшены на сервере Matrix, но в локальной БД
				// они помечены как отправленные, сбрасываем статус shared в БД и пробуем снова.
				if strings.Contains(err.Error(), "olm account is marked as shared") {
					slog.Warn("Olm account is marked as shared but keys disappeared from server. Resetting shared status in database to trigger re-upload...", "account", accountID)
					_, updateErr := rawDB.Exec("UPDATE crypto_account SET shared = 0")
					if updateErr != nil {
						slog.Error("Failed to reset shared status in crypto_account", "error", updateErr)
					} else {
						// Повторная попытка инициализации с теми же ключами
						err = helper.Init(context.Background())
					}
				}
			}
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

// formatJSONValue рекурсивно форматирует элементы JSON (карты, массивы, примитивы) с отступами.
// Для пустых карт и массивов возвращает строковые литералы {} и [] соответственно.
func formatJSONValue(val interface{}, indent int) string {
	indentStr := strings.Repeat("  ", indent)
	switch v := val.(type) {
	case map[string]interface{}:
		// Возвращаем {} для пустых карт
		if len(v) == 0 {
			return "{}"
		}
		var keys []string
		for k := range v {
			keys = append(keys, k)
		}
		// Сортируем ключи для предсказуемого и аккуратного вывода структуры
		sort.Strings(keys)

		var lines []string
		// Если на верхнем уровне есть поле "text", выводим его первым без имени ключа
		if indent == 0 {
			if textVal, hasText := v["text"]; hasText {
				valStr := formatJSONValue(textVal, indent)
				if valStr != "" {
					lines = append(lines, valStr)
					// Если text оказался сложным многострочным объектом, отделяем его пустой строкой
					if strings.Contains(valStr, "\n") {
						lines = append(lines, "")
					}
				}
			}
		}

		for _, k := range keys {
			if indent == 0 && k == "text" {
				continue
			}
			valStr := formatJSONValue(v[k], indent+1)
			
			// Проверяем, является ли вложенный элемент сложным непустым объектом (мапой или массивом)
			isComplex := false
			switch concrete := v[k].(type) {
			case map[string]interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			case []interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			}

			// Если вложенный элемент сложный или многострочный, переносим его на новую строку с отступом
			if isComplex || strings.Contains(valStr, "\n") || valStr == "" {
				lines = append(lines, fmt.Sprintf("%s%s:\n%s", indentStr, k, valStr))
			} else {
				lines = append(lines, fmt.Sprintf("%s%s: %s", indentStr, k, valStr))
			}
		}
		return strings.Join(lines, "\n")

	case []interface{}:
		// Возвращаем [] для пустых массивов
		if len(v) == 0 {
			return "[]"
		}
		var lines []string
		for _, item := range v {
			itemStr := formatJSONValue(item, indent+1)
			trimmedItemStr := strings.TrimSpace(itemStr)
			// Форматируем элементы списка с дефисом на уровне текущего отступа
			lines = append(lines, fmt.Sprintf("%s- %s", indentStr, trimmedItemStr))
		}
		return strings.Join(lines, "\n")

	default:
		// Для простых значений возвращаем строковое представление
		return fmt.Sprintf("%v", v)
	}
}

var credsRegex = regexp.MustCompile(`(?i)([a-zA-Z0-9+-.]+://)?([^/:\s]+:[^/@\s]+@|[^/@\s]+@)`)
var passParamRegex = regexp.MustCompile(`(?i)(pass(?:word)?|pwd|secret)=[^&\s"']+`)

// maskCredentialsInText маскирует любые учетные данные в тексте ошибок URL и параметрах (например, http://user:pass@host -> http://***@host, password=123 -> password=***)
func maskCredentialsInText(text string) string {
	text = credsRegex.ReplaceAllString(text, "${1}***@")
	text = passParamRegex.ReplaceAllString(text, "${1}=***")
	return text
}

// formatOrderedJSONValue форматирует слайс JSONPair в текстовый вид с отступами, сохраняя исходный порядок ключей.
// При indent == 0, если присутствует ключ "text", его значение выводится первым без префикса "text:".
func formatOrderedJSONValue(pairs []JSONPair, indent int) string {
	indentStr := strings.Repeat("  ", indent)
	var lines []string

	// Если на верхнем уровне есть поле "text", выводим его первым без имени ключа
	if indent == 0 {
		var textFieldVal interface{}
		var hasText bool
		for _, pair := range pairs {
			if pair.Key == "text" {
				textFieldVal = pair.Value
				hasText = true
				break
			}
		}

		if hasText {
			valStr := formatAnyJSONValue(textFieldVal, indent)
			if valStr != "" {
				lines = append(lines, valStr)
				// Если text оказался сложной многострочной структурой, отделяем его пустой строкой
				if strings.Contains(valStr, "\n") {
					lines = append(lines, "")
				}
			}
		}

		for _, pair := range pairs {
			if pair.Key == "text" {
				continue
			}
			valStr := formatAnyJSONValue(pair.Value, indent+1)
			
			isComplex := false
			switch concrete := pair.Value.(type) {
			case map[string]interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			case []interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			case []JSONPair:
				if len(concrete) > 0 {
					isComplex = true
				}
			}

			// Если вложенный элемент сложный или многострочный, переносим его на новую строку с отступом
			if isComplex || strings.Contains(valStr, "\n") || valStr == "" {
				lines = append(lines, fmt.Sprintf("%s%s:\n%s", indentStr, pair.Key, valStr))
			} else {
				lines = append(lines, fmt.Sprintf("%s%s: %s", indentStr, pair.Key, valStr))
			}
		}
	} else {
		for _, pair := range pairs {
			valStr := formatAnyJSONValue(pair.Value, indent+1)
			
			isComplex := false
			switch concrete := pair.Value.(type) {
			case map[string]interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			case []interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			case []JSONPair:
				if len(concrete) > 0 {
					isComplex = true
				}
			}

			// Если вложенный элемент сложный или многострочный, переносим его на новую строку с отступом
			if isComplex || strings.Contains(valStr, "\n") || valStr == "" {
				lines = append(lines, fmt.Sprintf("%s%s:\n%s", indentStr, pair.Key, valStr))
			} else {
				lines = append(lines, fmt.Sprintf("%s%s: %s", indentStr, pair.Key, valStr))
			}
		}
	}
	return strings.Join(lines, "\n")
}

// formatAnyJSONValue форматирует произвольное значение (вложенные map/slice/примитивы)
func formatAnyJSONValue(val interface{}, indent int) string {
	indentStr := strings.Repeat("  ", indent)
	switch v := val.(type) {
	case map[string]interface{}:
		if len(v) == 0 {
			return "{}"
		}
		// Поскольку это вложенная map, порядок не гарантирован, выводим с сортировкой
		var keys []string
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var lines []string
		for _, k := range keys {
			valStr := formatAnyJSONValue(v[k], indent+1)
			
			isComplex := false
			switch concrete := v[k].(type) {
			case map[string]interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			case []interface{}:
				if len(concrete) > 0 {
					isComplex = true
				}
			}

			if isComplex || strings.Contains(valStr, "\n") || valStr == "" {
				lines = append(lines, fmt.Sprintf("%s%s:\n%s", indentStr, k, valStr))
			} else {
				lines = append(lines, fmt.Sprintf("%s%s: %s", indentStr, k, valStr))
			}
		}
		return strings.Join(lines, "\n")

	case []interface{}:
		if len(v) == 0 {
			return "[]"
		}
		var lines []string
		for _, item := range v {
			itemStr := formatAnyJSONValue(item, indent+1)
			lines = append(lines, fmt.Sprintf("%s- %s", indentStr, strings.TrimSpace(itemStr)))
		}
		return strings.Join(lines, "\n")

	default:
		return fmt.Sprintf("%v", v)
	}
}

// XMLPair используется для сохранения исходного порядка тегов при парсинге XML.
type XMLPair struct {
	Key   string
	Value interface{}
}

// parseXML преобразует байты XML-документа в древовидную структуру map/slice.
// Чтение начинается с поиска первого тега (StartElement).
func parseXML(body []byte) (interface{}, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if start, ok := t.(xml.StartElement); ok {
			val, err := parseXMLElement(dec, start)
			if err != nil {
				return nil, err
			}
			// Возвращаем карту, где ключом является имя корневого тега
			return map[string]interface{}{start.Name.Local: val}, nil
		}
	}
}

// parseXMLElement рекурсивно парсит XML-элемент и его дочерние узлы.
func parseXMLElement(dec *xml.Decoder, start xml.StartElement) (interface{}, error) {
	var children []XMLPair
	var textVal string

	for {
		t, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		switch tok := t.(type) {
		case xml.StartElement:
			// Рекурсивно обрабатываем вложенный элемент
			childVal, err := parseXMLElement(dec, tok)
			if err != nil {
				return nil, err
			}
			children = append(children, XMLPair{Key: tok.Name.Local, Value: childVal})

		case xml.CharData:
			// Накапливаем текстовое содержимое элемента
			textVal += string(tok)

		case xml.EndElement:
			// Если дошли до закрывающего тега текущего уровня, завершаем обработку
			if tok.Name.Local == start.Name.Local {
				if len(children) == 0 {
					return strings.TrimSpace(textVal), nil
				}
				// Преобразуем дочерние элементы в карту с группировкой повторяющихся ключей
				return convertXMLPairsToMap(children), nil
			}
		}
	}
	return strings.TrimSpace(textVal), nil
}

// convertXMLPairsToMap группирует одноименные элементы в массивы и строит map[string]interface{}.
func convertXMLPairsToMap(pairs []XMLPair) map[string]interface{} {
	m := make(map[string]interface{})
	counts := make(map[string]int)
	for _, p := range pairs {
		counts[p.Key]++
	}

	for _, p := range pairs {
		if counts[p.Key] > 1 {
			// Если имя тега повторяется, преобразуем его в массив элементов
			if _, exists := m[p.Key]; !exists {
				m[p.Key] = []interface{}{p.Value}
			} else {
				m[p.Key] = append(m[p.Key].([]interface{}), p.Value)
			}
		} else {
			m[p.Key] = p.Value
		}
	}
	return m
}

// stripXMLRoot извлекает внутреннее содержимое, если корневой тег является единственным контейнером.
// Это позволяет убрать теги вроде <response>...</response> и вывести только полезные поля.
func stripXMLRoot(val interface{}) interface{} {
	m, ok := val.(map[string]interface{})
	if !ok || len(m) != 1 {
		return val
	}
	for _, v := range m {
		switch concrete := v.(type) {
		case map[string]interface{}, []interface{}:
			return concrete
		}
	}
	return val
}

// truncateText обрезает строку до максимальной длины в символах (runes) и добавляет уведомление об обрезке.
func truncateText(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	suffixPlaceholder := fmt.Sprintf("\n... [Ответ обрезан, показано %d из %d символов]", maxLen, len(runes))
	suffixRunesLen := len([]rune(suffixPlaceholder))
	// Уменьшаем maxLen на размер суффикса, чтобы результирующий вывод гарантированно не превышал лимит.
	adjustedMax := maxLen - suffixRunesLen
	if adjustedMax <= 0 {
		return suffixPlaceholder
	}
	if len(runes) <= adjustedMax {
		return text
	}
	// Пересчитываем суффикс с точным значением adjustedMax
	suffix := fmt.Sprintf("\n... [Ответ обрезан, показано %d из %d символов]", adjustedMax, len(runes))
	return string(runes[:adjustedMax]) + suffix
}

// isTextContent определяет, является ли содержимое текстовым (JSON, XML, HTML, текст)
// или бинарными данными (изображения, архивы и т.д.) на основе заголовка Content-Type и сигнатуры байт.
func isTextContent(contentType string, body []byte) bool {
	ct := strings.ToLower(contentType)
	if ct != "" {
		// Доверяем явному текстовому заголовку
		if strings.Contains(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml") || strings.Contains(ct, "javascript") {
			return true
		}
		// Детектируем известные бинарные форматы
		if strings.Contains(ct, "image/") || strings.Contains(ct, "octet-stream") || strings.Contains(ct, "zip") || strings.Contains(ct, "pdf") {
			return false
		}
	}

	// Анализируем первые 512 байт тела для дополнительного определения типа
	detectLen := len(body)
	if detectLen > 512 {
		detectLen = 512
	}
	if detectLen > 0 {
		detected := strings.ToLower(http.DetectContentType(body[:detectLen]))
		if strings.Contains(detected, "text/") || strings.Contains(detected, "json") || strings.Contains(detected, "xml") {
			return true
		}
		if strings.Contains(detected, "image/") || strings.Contains(detected, "octet-stream") || strings.Contains(detected, "zip") || strings.Contains(detected, "pdf") {
			return false
		}
	}

	// Проверяем наличие нулевых байтов, которые характерны для бинарных файлов
	for i := 0; i < detectLen; i++ {
		if body[i] == 0x00 {
			return false
		}
	}
	return true
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
	configMu.RLock()
	if globalConfig == nil {
		configMu.RUnlock()
		slog.Warn("Global configuration is nil, skipping Matrix command processing", "roomID", evt.RoomID)
		return
	}

	menuID := conf.Menu
	if menuID == "" {
		configMu.RUnlock()
		return
	}

	var targetMenu *Menu
	for i := range globalConfig.Menus {
		if globalConfig.Menus[i].ID == menuID {
			m := globalConfig.Menus[i]
			m.Items = append([]MenuItem(nil), globalConfig.Menus[i].Items...)
			targetMenu = &m
			break
		}
	}
	configMu.RUnlock()

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
			go executeMenuCommand(inst, *matchedItem)
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
	configMu.RLock()
	if globalConfig == nil {
		configMu.RUnlock()
		slog.Warn("Global configuration is nil, skipping Matrix reaction processing", "roomID", evt.RoomID)
		return
	}

	var targetMenu *Menu
	for i := range globalConfig.Menus {
		if globalConfig.Menus[i].ID == conf.Menu {
			m := globalConfig.Menus[i]
			m.Items = append([]MenuItem(nil), globalConfig.Menus[i].Items...)
			targetMenu = &m
			break
		}
	}
	configMu.RUnlock()

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

		if targetMenu.Items[i].Reaction != "" && cleanedTarget != "" && cleanedIncoming != "" && cleanedTarget == cleanedIncoming {
			matchedItem = &targetMenu.Items[i]
			break
		}
	}

	// Если команда найдена по эмодзи
	if matchedItem != nil {
		go executeMenuCommand(inst, *matchedItem)
	} else {
		// Команда по эмодзи не найдена
		slog.Debug("Matrix reaction: no matching command for emoji", "emoji", emoji)
	}
}

// executeMenuCommand выполняет команду меню (локальный скрипт или HTTP-запрос) и отправляет ответ в Matrix.
func executeMenuCommand(inst *Instance, item MenuItem) {
	// 1. Если настроен локальный скрипт (имеет приоритет над URL)
	if item.Script != "" {
		slog.Info("Executing matched command script from Matrix", "command", item.Name, "script", item.Script)

		scriptPath := filepath.Join("/app/scripts", item.Script)

		// Проверяем существование файла скрипта
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			slog.Error("Script file does not exist", "path", scriptPath, "command", item.Name)
			_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s", item.Name))
			return
		}

		// Получаем таймаут выполнения из переменной окружения SCRIPT_TIMEOUT (по умолчанию 15 секунд)
		timeoutSec := 15
		if envVal := os.Getenv("SCRIPT_TIMEOUT"); envVal != "" {
			if parsed, err := strconv.Atoi(envVal); err == nil && parsed > 0 {
				timeoutSec = parsed
			}
		}
		timeout := time.Duration(timeoutSec) * time.Second

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "sh", scriptPath)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err != nil {
			slog.Error("Failed to execute command script", "command", item.Name, "script", item.Script, "error", err, "stderr", stderr.String())
			_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s", item.Name))
			return
		}

		outputBytes := stdout.Bytes()
		slog.Info("Command script executed successfully", "command", item.Name, "outputLen", len(outputBytes))

		if len(outputBytes) > 0 && strings.TrimSpace(string(outputBytes)) != "" {
			// Проверяем, является ли вывод текстовым или бинарным (изображение/медиа)
			if isTextContent("", outputBytes) {
				// Пытаемся распарсить вывод как упорядоченный JSON для сохранения порядка полей
				if pairs, err := DecodeOrderedJSON(bytes.NewReader(outputBytes)); err == nil {
					formatted := formatOrderedJSONValue(pairs, 0)
					_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(formatted, 4000)))
				} else {
					// Если это другой валидный JSON (массив, примитив) или обычный текст/XML
					var jsonVal interface{}
					if jsonErr := json.Unmarshal(outputBytes, &jsonVal); jsonErr == nil {
						switch concreteVal := jsonVal.(type) {
						case []interface{}:
							formatted := formatAnyJSONValue(concreteVal, 0)
							_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(formatted, 4000)))
						default:
							_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(string(outputBytes), 4000)))
						}
					} else if xmlVal, xmlErr := parseXML(outputBytes); xmlErr == nil {
						stripped := stripXMLRoot(xmlVal)
						formatted := formatJSONValue(stripped, 0)
						_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(formatted, 4000)))
					} else {
						_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(string(outputBytes), 4000)))
					}
				}
			} else {
				// Если вывод бинарный (например, изображение), сохраняем как временный файл и отправляем
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

				tempFileName := fmt.Sprintf("script_%s_%d%s", item.Name, time.Now().UnixNano(), ext)
				tempFilePath := filepath.Join(tempDir, tempFileName)

				if writeErr := os.WriteFile(tempFilePath, outputBytes, 0644); writeErr != nil {
					slog.Error("Failed to save script output binary to temp file", "error", writeErr)
					_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s", item.Name))
					return
				}

				// Отправляем как вложение
				go func(filePath, fileName, mime string) {
					defer os.Remove(filePath) // Удаляем файл после отправки
					slog.Info("Sending script output binary as Matrix file", "fileName", fileName, "mime", mime)
					if sendErr := sendMatrixWithRetry(inst, fmt.Sprintf("✅ Результат команды !%s (бинарный вывод)", item.Name), filePath, fileName, mime); sendErr != nil {
						slog.Error("Failed to send script binary output to Matrix", "error", sendErr)
					}
				}(tempFilePath, tempFileName, mimeType)
			}
		} else {
			_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно (вывод пуст)", item.Name))
		}
		return
	}

	// 2. Если настроен URL
	if item.URL != "" {
		slog.Info("Executing matched command via HTTP GET from Matrix", "command", item.Name, "url", item.URL)

		httpClient := &http.Client{
			Timeout: 15 * time.Second,
		}

		resp, err := httpClient.Get(item.URL)
		if err != nil {
			slog.Error("Failed to execute command via HTTP GET", "command", item.Name, "url", item.URL, "error", err)
			_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s", item.Name))
			return
		}
		defer resp.Body.Close()

		slog.Info("Command executed successfully", "command", item.Name, "status", resp.Status)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			limitReader := io.LimitReader(resp.Body, 1024*1024)
			bodyBytes, readErr := io.ReadAll(limitReader)
			if readErr == nil && len(bodyBytes) > 0 && strings.TrimSpace(string(bodyBytes)) != "" && isTextContent(resp.Header.Get("Content-Type"), bodyBytes) {
				// Пытаемся распарсить вывод как упорядоченный JSON для сохранения порядка полей
				if pairs, err := DecodeOrderedJSON(bytes.NewReader(bodyBytes)); err == nil {
					formatted := formatOrderedJSONValue(pairs, 0)
					_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(formatted, 4000)))
				} else {
					var jsonVal interface{}
					if jsonErr := json.Unmarshal(bodyBytes, &jsonVal); jsonErr == nil {
						switch concreteVal := jsonVal.(type) {
						case []interface{}:
							formatted := formatAnyJSONValue(concreteVal, 0)
							_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(formatted, 4000)))
						default:
							_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(string(bodyBytes), 4000)))
						}
					} else if xmlVal, xmlErr := parseXML(bodyBytes); xmlErr == nil {
						stripped := stripXMLRoot(xmlVal)
						formatted := formatJSONValue(stripped, 0)
						_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(formatted, 4000)))
					} else {
						_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно:\n%s", item.Name, truncateText(string(bodyBytes), 4000)))
					}
				}
			} else {
				_ = sendMatrixResponse(inst, fmt.Sprintf("✅ Команда !%s выполнена успешно", item.Name))
			}
		} else {
			slog.Error("Command returned non-successful HTTP status", "command", item.Name, "status", resp.Status)
			_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s", item.Name))
		}
		return
	}

	slog.Error("Command execution failed: neither script nor url configured", "command", item.Name)
	_ = sendMatrixResponse(inst, fmt.Sprintf("❌ Ошибка при выполнении команды !%s", item.Name))
}

// sendMatrixResponse отправляет мгновенное текстовое сообщение в комнату Matrix в обход очереди.
func sendMatrixResponse(inst *Instance, text string) error {
	return sendMatrixWithRetry(inst, html.UnescapeString(maskCredentialsInText(text)), "", "", "")
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
			// Fail-Fast: если нет учетных данных, ретраи бессмысленны
			if strings.Contains(err.Error(), "no credentials for Matrix account") {
				return err
			}
			if attempt < retryCount {
				time.Sleep(delay)
				delay *= 2
				continue
			}
			return fmt.Errorf("failed to get Matrix client: %w", err)
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
				var userIDs []id.UserID
				roomMembersMu.Lock()
				cached, found := roomMembersCache[roomID]
				roomMembersMu.Unlock()

				// Проверяем TTL кэша (1 час)
				if found && time.Since(cached.cachedAt) < 1*time.Hour {
					userIDs = cached.userIDs
				} else {
					found = false
				}

				if !found {
					slog.Info("Fetching room members for E2EE", "roomID", roomID, "account", accountID)
					members, mErr := client.JoinedMembers(context.Background(), roomID)
					if mErr == nil {
						userIDs = make([]id.UserID, 0, len(members.Joined))
						for userID := range members.Joined {
							userIDs = append(userIDs, userID)
						}
						roomMembersMu.Lock()
						roomMembersCache[roomID] = cachedMembers{
							userIDs:  userIDs,
							cachedAt: time.Now(),
						}
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
					// Принудительный сброс исходящей Megolm-сессии при старте удален, так как он приводил
					// к постоянной генерации новых сессий при каждом перезапуске бота, из-за чего
					// клиенты получателей не могли вовремя получить новые ключи или блокировали их.
					// Теперь мы полностью полагаемся на существующую в БД сессию и стандартный
					// жизненный цикл ротации сессий библиотеки mautrix-go.


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
			slog.Error("Matrix credentials invalid, performing Fail-Fast abort", "account", accountID, "error", merr.Error())
			return fmt.Errorf("Matrix credentials invalid: %w", err)
		}

		if attempt < retryCount {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return fmt.Errorf("failed to send Matrix message after %d attempts", retryCount)
}
