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
}

// DBWrapper – обертка над sql.DB.
type DBWrapper struct {
	db *sql.DB
}

// InitDB инициализирует базу данных и создает таблицу outbox.
func InitDB(path string) (*DBWrapper, error) {
	slog.Info("Starting database optimization (VACUUM/ANALYZE)")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Настройка WAL режима и таймаута для Docker/macOS/Linux.
	if _, err := db.Exec(`
		PRAGMA journal_mode=WAL;
		PRAGMA synchronous=NORMAL;
		PRAGMA busy_timeout=5000;
	`); err != nil {
		return nil, fmt.Errorf("failed to set SQLite pragmas: %v", err)
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

	// Оптимизация при старте.
	db.Exec("VACUUM;")
	db.Exec("ANALYZE;")
	slog.Info("Database optimization completed")

	return &DBWrapper{db: db}, nil
}

// SaveMessage сохраняет сообщение в очередь.
func (db *DBWrapper) SaveMessage(m *Message) error {
	query := `INSERT INTO outbox (instance_name, service, payload, status, attempts, ttl_deadline, created_at) 
	          VALUES (?, ?, ?, ?, ?, ?, ?)`
	res, err := db.db.Exec(query,
		m.InstanceName,
		m.Service,
		m.Payload,
		m.Status,
		m.Attempts,
		m.TTLDeadline.Unix(), // Сохраняем как секунды
		m.CreatedAt.Unix(),   // Сохраняем как секунды
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
	query := `SELECT id, instance_name, service, payload, status, attempts, ttl_deadline, created_at 
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
		err := rows.Scan(&m.ID, &m.InstanceName, &m.Service, &m.Payload, &m.Status, &m.Attempts, &ttlUnix, &createdUnix)
		if err != nil {
			return nil, err
		}
		// Восстанавливаем время из секунд и переводим в локальное время контейнера.
		m.TTLDeadline = time.Unix(ttlUnix, 0).Local()
		m.CreatedAt = time.Unix(createdUnix, 0).Local()
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
