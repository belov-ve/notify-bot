package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite"
)

// Message представляет структуру сообщения в БД.
type Message struct {
	ID           int64
	InstanceName string
	Service      string // telegram, matrix
	Payload      string
	Status       string // pending, sent, failed
	Attempts     int
	TTLDeadline  time.Time
	CreatedAt    time.Time
	FilePath     string // Path to locally saved file on disk
	FileName     string // Original filename
	MimeType     string // MIME type of the file
}

// DBWrapper – обертка над sql.DB.
type DBWrapper struct {
	db *sql.DB
}

// InitDB инициализирует базу данных и создает таблицу outbox.
func InitDB(path string) (*DBWrapper, error) {
	slog.Info("Starting database optimization (VACUUM/ANALYZE)")
	// Используем URI DSN для автоматического применения PRAGMA ко всем соединениям в пуле.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Ограничение до 1 соединения — критично для SQLite в Go, чтобы избежать "database is locked".
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Создание таблицы. Храним время как BIGINT (Unix Timestamp) для точности.
	query := `
	CREATE TABLE IF NOT EXISTS outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		instance_name TEXT NOT NULL,
		service TEXT NOT NULL,
		payload TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER DEFAULT 0,
		ttl_deadline BIGINT NOT NULL,
		created_at BIGINT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_status ON outbox(status);
	`
	if _, err := db.Exec(query); err != nil {
		return nil, err
	}

	// 2.2. Проверка и миграция схемы для поддержки вложений (картинок и файлов)
	rows, err := db.Query("PRAGMA table_info(outbox)")
	if err != nil {
		return nil, fmt.Errorf("failed to query table info: %v", err)
	}

	hasFilePath := false
	hasFileName := false
	hasMimeType := false

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltVal sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltVal, &pk); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan table info: %v", err)
		}
		switch name {
		case "file_path":
			hasFilePath = true
		case "file_name":
			hasFileName = true
		case "mime_type":
			hasMimeType = true
		}
	}
	rows.Close() // Закрываем rows перед выполнением ALTER TABLE

	if !hasFilePath {
		slog.Info("Migrating DB: adding file_path column to outbox table")
		if _, err := db.Exec("ALTER TABLE outbox ADD COLUMN file_path TEXT"); err != nil {
			return nil, fmt.Errorf("failed to add file_path column: %v", err)
		}
	}
	if !hasFileName {
		slog.Info("Migrating DB: adding file_name column to outbox table")
		if _, err := db.Exec("ALTER TABLE outbox ADD COLUMN file_name TEXT"); err != nil {
			return nil, fmt.Errorf("failed to add file_name column: %v", err)
		}
	}
	if !hasMimeType {
		slog.Info("Migrating DB: adding mime_type column to outbox table")
		if _, err := db.Exec("ALTER TABLE outbox ADD COLUMN mime_type TEXT"); err != nil {
			return nil, fmt.Errorf("failed to add mime_type column: %v", err)
		}
	}

	// Оптимизация при старте.
	db.Exec("VACUUM;")
	db.Exec("ANALYZE;")
	slog.Info("Database optimization completed")

	return &DBWrapper{db: db}, nil
}

// SaveMessage сохраняет сообщение в очередь.
func (db *DBWrapper) SaveMessage(m *Message) error {
	// Save attachment fields (file_path, file_name, mime_type) to the database.
	query := `INSERT INTO outbox (instance_name, service, payload, status, attempts, ttl_deadline, created_at, file_path, file_name, mime_type) 
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := db.db.Exec(query,
		m.InstanceName,
		m.Service,
		m.Payload,
		m.Status,
		m.Attempts,
		m.TTLDeadline.Unix(), // Сохраняем как секунды
		m.CreatedAt.Unix(),   // Сохраняем как секунды
		m.FilePath,
		m.FileName,
		m.MimeType,
	)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	m.ID = id
	return nil
}

// GetPendingMessages извлекает сообщения, ожидающие отправки.
func (db *DBWrapper) GetPendingMessages() ([]Message, error) {
	// Select attachment fields (file_path, file_name, mime_type) for sending attachments.
	query := `SELECT id, instance_name, service, payload, status, attempts, ttl_deadline, created_at, file_path, file_name, mime_type 
	          FROM outbox WHERE status != 'sent' ORDER BY attempts ASC, created_at ASC`
	rows, err := db.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var ttlUnix, createdUnix int64
		var filePath, fileName, mimeType sql.NullString
		err := rows.Scan(&m.ID, &m.InstanceName, &m.Service, &m.Payload, &m.Status, &m.Attempts, &ttlUnix, &createdUnix, &filePath, &fileName, &mimeType)
		if err != nil {
			return nil, err
		}
		// Восстанавливаем время из секунд и переводим в локальное время контейнера.
		m.TTLDeadline = time.Unix(ttlUnix, 0).Local()
		m.CreatedAt = time.Unix(createdUnix, 0).Local()
		m.FilePath = filePath.String
		m.FileName = fileName.String
		m.MimeType = mimeType.String
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// UpdateMessageStatus обновляет статус и количество попыток.
func (db *DBWrapper) UpdateMessageStatus(id int64, status string, attempts int) error {
	query := `UPDATE outbox SET status = ?, attempts = ? WHERE id = ?`
	_, err := db.db.Exec(query, status, attempts, id)
	return err
}

// DeleteMessage удаляет сообщение (после успешной отправки).
func (db *DBWrapper) DeleteMessage(id int64) error {
	query := `DELETE FROM outbox WHERE id = ?`
	_, err := db.db.Exec(query, id)
	return err
}

// IsFileReferenced проверяет, ссылаются ли другие сообщения на данный файл.
// Это необходимо при параллельной отправке в несколько каналов (Telegram и Matrix),
// чтобы не удалить файл с диска раньше времени, пока его не отправят все сервисы.
func (db *DBWrapper) IsFileReferenced(filePath string, excludeMsgID int64) (bool, error) {
	if filePath == "" {
		return false, nil
	}
	var count int
	query := `SELECT COUNT(*) FROM outbox WHERE file_path = ? AND id != ?`
	err := db.db.QueryRow(query, filePath, excludeMsgID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetQueueStats возвращает количество сообщений в очереди для каждого инстанса.
func (db *DBWrapper) GetQueueStats() (map[string]int, error) {
	query := `SELECT instance_name, COUNT(*) FROM outbox GROUP BY instance_name`
	rows, err := db.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make(map[string]int)
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err == nil {
			stats[name] = count
		}
	}
	return stats, nil
}

// Close закрывает соединение с БД.
func (db *DBWrapper) Close() error {
	return db.db.Close()
}
