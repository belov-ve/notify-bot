package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"go.mau.fi/util/dbutil"

	_ "modernc.org/sqlite"
)

const AppVersion = "2.2.3"

// matrixClients и другие мапы теперь индексируются по "Account ID" (slug от username + homeserver)
var (
	matrixClients    = make(map[string]*mautrix.Client)
	cryptoHelpers    = make(map[string]*cryptohelper.CryptoHelper)
	matrixDBs        = make(map[string]*sql.DB)
	matrixMu         sync.Mutex
	
	roomMembersCache = make(map[id.RoomID][]id.UserID)
	roomMembersMu    sync.Mutex
	
	sessionResetCache = make(map[id.RoomID]bool)
	sessionResetMu    sync.Mutex
)

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
}

func getMatrixClient(inst *Instance) (*mautrix.Client, error) {
	conf := inst.Matrix
	accountID := getAccountID(conf.Username, conf.Homeserver)

	matrixMu.Lock()
	defer matrixMu.Unlock()

	if client, ok := matrixClients[accountID]; ok {
		return client, nil
	}

	client, err := mautrix.NewClient(conf.Homeserver, "", "")
	if err != nil {
		return nil, err
	}

	dbDir := "/app/data"
	if os.Getenv("DB_PATH") != "" {
		dbDir = filepath.Dir(os.Getenv("DB_PATH"))
	}
	cryptoDBPath := filepath.Join(dbDir, fmt.Sprintf("%s.db", accountID))
	
	dsn := fmt.Sprintf("file:%s?_busy_timeout=10000&_journal_mode=WAL&_sync=NORMAL", cryptoDBPath)
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	rawDB.SetMaxOpenConns(1)
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
			return nil, err
		}
		client.AccessToken = resp.AccessToken
		client.UserID = resp.UserID
		client.DeviceID = resp.DeviceID
		saveSessionInfo(rawDB, string(client.UserID), client.AccessToken)
	} else {
		return nil, fmt.Errorf("no credentials for Matrix account %s", accountID)
	}

	// Оставляем попытку установить имя, но без лишних заголовков и "хитростей"
	_ = client.SetDeviceInfo(context.Background(), client.DeviceID, &mautrix.ReqDeviceInfo{DisplayName: deviceDisplayName})

	if conf.Encryption {
		slog.Info("Initializing Matrix E2EE", "account", accountID)
		
		cryptoDB, err := dbutil.NewWithDB(rawDB, "sqlite")
		if err != nil {
			return nil, err
		}

		helper, err := cryptohelper.NewCryptoHelper(client, []byte("pickle-"+accountID), cryptoDB)
		if err != nil {
			return nil, err
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
			return nil, err
		}

		_ = helper.Machine().ShareKeys(context.Background(), 0)
		helper.Machine().ShareKeysMinTrust = id.TrustStateUnset
		
		// Настоящий Self-healing: Разрешаем клиентам запрашивать ключи.
		// Если у клиента произошел сбой, или он был оффлайн во время обмена ключами,
		// он автоматически пришлет m.room_key_request.
		// Возвращая nil, мы разрешаем боту прозрачно отправлять недостающие ключи сессий.
		helper.Machine().AllowKeyShare = func(ctx context.Context, device *id.Device, info event.RequestedKeyInfo) *crypto.KeyShareRejection {
			slog.Info("Key share requested by device", "user", device.UserID, "device", device.DeviceID, "sessionID", info.SessionID)
			return nil // Разрешаем отправку ключа
		}
		
		fingerprint := helper.Machine().OwnIdentity().Fingerprint()
		var formattedFingerprint string
		for i, r := range fingerprint {
			if i > 0 && i%4 == 0 { formattedFingerprint += " " }
			formattedFingerprint += string(r)
		}
		slog.Info("Matrix device identity", "account", accountID, "device_id", client.DeviceID, "fingerprint", formattedFingerprint)

		client.Crypto = helper
		cryptoHelpers[accountID] = helper

		syncer := mautrix.NewDefaultSyncer()
		client.Syncer = syncer
		
		vhStore := verificationhelper.NewInMemoryVerificationStore()
		handler := &verificationHandler{accountID: accountID}
		vh := verificationhelper.NewVerificationHelper(client, helper.Machine(), vhStore, handler, false)
		handler.vh = vh
		_ = vh.Init(context.Background())

		// Обработчик всех событий для передачи to-device сообщений в crypto engine.
		// Это критически важно для работы E2EE (обмен ключами, верификация).
		syncer.OnEvent(func(ctx context.Context, evt *event.Event) {
			helper.Machine().HandleToDeviceEvent(ctx, evt)
		})

		// Обработчик изменения состава комнаты (вход/выход участников).
		// При любом изменении мы сбрасываем кэш участников для этой комнаты.
		// Это гарантирует, что при следующей отправке сообщения бот получит актуальный список
		// устройств (включая новых участников) и «дошлет» им ключи сессии.
		syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
			roomMembersMu.Lock()
			delete(roomMembersCache, evt.RoomID)
			roomMembersMu.Unlock()
			slog.Info("Matrix room membership changed, member cache invalidated", 
				"roomID", evt.RoomID, 
				"userID", evt.GetStateKey(),
				"account", accountID)
		})

		go func(c *mautrix.Client, accID string, db *sql.DB) {
			slog.Debug("Starting Matrix sync loop", "account", accID)
			since := loadSyncToken(db)
			for {
				resp, err := c.SyncRequest(context.Background(), 30, since, "", false, event.PresenceOnline)
				if err != nil {
					if merr, ok := err.(mautrix.HTTPError); ok && (merr.IsStatus(401) || merr.IsStatus(403)) {
						_, _ = db.Exec("DELETE FROM local_metadata WHERE key IN ('access_token', 'user_id')")
						ResetMatrixClient(accID)
						return 
					}
					time.Sleep(10 * time.Second)
					continue
				}
				c.Syncer.ProcessResponse(context.Background(), resp, since)
				since = resp.NextBatch
				saveSyncToken(accID, db, since)
			}
		}(client, accountID, rawDB)

		slog.Info("Matrix E2EE initialized successfully", "account", accountID)
	}

	matrixClients[accountID] = client
	return client, nil
}

func sendMatrixWithRetry(inst *Instance, text string) error {
	conf := inst.Matrix
	accountID := getAccountID(conf.Username, conf.Homeserver)
	retryCount := conf.RetryCount
	if retryCount <= 0 { retryCount = 3 }
	retryDelay := conf.RetryDelay
	if retryDelay <= 0 { retryDelay = 2 }

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
		content := &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    text,
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

				if err == nil && len(userIDs) > 0 {
					// Обязательно загружаем ключи устройств в локальную БД OlmMachine (иначе он не будет знать, кому отправлять ключи сессии)
					_, qErr := helper.Machine().FetchKeys(context.Background(), userIDs, true)
					if qErr != nil {
						slog.Warn("Failed to fetch device keys for room members", "roomID", roomID, "error", qErr)
						err = qErr
					}
				}

				if err == nil && len(userIDs) > 0 {
					// Автоматический сброс сессии (Self-healing) при первом сообщении после старта бота.
					// Это гарантирует очистку "битых" сессий без ручного вмешательства в БД.
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
					} else if err != nil {
						slog.Warn("Failed to share group session, retrying with session reset", "roomID", roomID, "error", err)
						_ = helper.Machine().CryptoStore.RemoveOutboundGroupSession(context.Background(), roomID)
						err = helper.Machine().ShareGroupSession(context.Background(), roomID, userIDs)
						if err != nil && strings.Contains(err.Error(), "group session already shared") {
							err = nil
						}
					}
				}

				if err == nil {
					var encryptedContent *event.EncryptedEventContent
					encryptedContent, err = helper.Encrypt(context.Background(), roomID, event.EventMessage, content)
					if err == nil {
						resp, err = client.SendMessageEvent(context.Background(), roomID, event.EventEncrypted, encryptedContent)
					}
				}
			} else {
				err = fmt.Errorf("crypto helper not found for %s", accountID)
			}
		} else {
			resp, err = client.SendMessageEvent(context.Background(), roomID, event.EventMessage, content)
		}

		if err == nil && resp != nil {
			slog.Info("Matrix message sent", "account", accountID, "eventID", resp.EventID)
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
