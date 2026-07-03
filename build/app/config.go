package main

import (
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

// Instance описывает конфигурацию одного порта (инстанса) бота.
type Instance struct {
	Name          string          `yaml:"name"`
	Port          int             `yaml:"port"`
	Enabled       bool            `yaml:"enabled"`
	AllowedIPs    []string        `yaml:"allowed_ips"`
	TTL           int             `yaml:"ttl"`            // Время жизни сообщения в секундах
	BlockDelivery bool            `yaml:"block_delivery"` // Блокировка отправки сообщений
	ShowTime      bool            `yaml:"show_time"`      // Добавлять метку времени к каждому сообщению
	Telegram      *TelegramConfig `yaml:"telegram"`
	Matrix        *MatrixConfig   `yaml:"matrix"`
	Tasks         string          `yaml:"tasks"`          // ID подключенного списка задач по расписанию
}

// TelegramConfig – настройки для отправки в Telegram.
type TelegramConfig struct {
	Enabled    bool   `yaml:"enabled"`
	BotToken   string `yaml:"bot_token"`
	ChatID     string `yaml:"chat_id"`
	RetryCount int    `yaml:"retry_count"`
	RetryDelay int    `yaml:"retry_delay"`
	Menu       string `yaml:"menu"` // Подключенное меню для Telegram
}

// MatrixConfig – настройки для отправки в Matrix.
type MatrixConfig struct {
	Enabled     bool             `yaml:"enabled"`
	Homeserver  string           `yaml:"homeserver"`
	RoomID      string           `yaml:"room_id"`
	AccessToken string           `yaml:"access_token"`
	Username    string           `yaml:"username"`
	Password    string           `yaml:"password"`
	RetryCount  int              `yaml:"retry_count"`
	RetryDelay  int              `yaml:"retry_delay"`
	Encryption  bool             `yaml:"encryption"`   // Включить сквозное шифрование (E2EE) для Matrix
	RecoveryKey string           `yaml:"recovery_key"` // Ключ восстановления для сквозного шифрования (E2EE)
	KeyBackup   bool             `yaml:"key_backup"`   // Включение резервного копирования ключей E2EE на сервере Matrix
	Menu        string           `yaml:"menu"`         // Уникальный идентификатор подключенного меню
}

// IsKeyBackupEnabled возвращает true, если включено резервное копирование ключей E2EE.
// Приоритет отдается переменной окружения MATRIX_ENABLE_KEY_BACKUP.
func (m *MatrixConfig) IsKeyBackupEnabled() bool {
	if envVal := os.Getenv("MATRIX_ENABLE_KEY_BACKUP"); envVal != "" {
		return envVal == "true"
	}
	return m.KeyBackup
}

// MenuItem описывает один элемент меню интерактивных команд.
type MenuItem struct {
	Name        string `yaml:"name"`        // Имя команды (например, "snapshot")
	URL         string `yaml:"url"`         // URL адрес выполняемого HTTP GET запроса
	Script      string `yaml:"script"`      // Имя файла локального скрипта в /app/scripts/ (например, "status.sh")
	Description string `yaml:"description"` // Описание команды для вывода в чат
	Reaction    string `yaml:"reaction"`    // Эмодзи-реакция для быстрого запуска команды (например, "📸")
}

// Menu описывает структуру меню с уникальным идентификатором.
type Menu struct {
	ID    string     `yaml:"id"`    // Уникальный идентификатор меню
	Items []MenuItem `yaml:"items"` // Список элементов меню
}

// TaskItem описывает одну запланированную задачу для периодического выполнения.
type TaskItem struct {
	Name        string `yaml:"name"`        // Имя задачи (например, "disk_check")
	Enabled     *bool  `yaml:"enabled"`     // Флаг активности задачи (по умолчанию true, nil трактуется как true)
	Schedule    string `yaml:"schedule"`    // Расписание в формате cron (например, "*/5 * * * *")
	URL         string `yaml:"url"`         // URL выполняемого HTTP-запроса (опционально)
	Script      string `yaml:"script"`      // Имя файла локального скрипта (опционально)
	Description string `yaml:"description"` // Описание задачи для логирования (опционально)
}

// TaskList описывает именованный список запланированных задач.
type TaskList struct {
	ID    string     `yaml:"id"`    // Уникальный идентификатор списка задач
	Items []TaskItem `yaml:"items"` // Список задач
}

// Config – корневая структура конфигурационного файла.
type Config struct {
	Instances []Instance `yaml:"instances"`
	Menus     []Menu     `yaml:"menus"` // Глобальная секция меню команд
	Tasks     []TaskList `yaml:"tasks"` // Глобальная секция задач по расписанию
}

// LoadConfig загружает и парсит YAML файл конфигурации.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Проставляем значения по умолчанию для ретраев и IP фильтрации.
	for i := range cfg.Instances {
		inst := &cfg.Instances[i]
		if len(inst.AllowedIPs) == 0 {
			inst.AllowedIPs = []string{"0.0.0.0/0"}
		}
		if inst.Telegram != nil {
			if inst.Telegram.RetryCount <= 0 {
				inst.Telegram.RetryCount = 3
			}
			if inst.Telegram.RetryDelay <= 0 {
				inst.Telegram.RetryDelay = 2
			}
		}
		if inst.Matrix != nil {
			if inst.Matrix.RetryCount <= 0 {
				inst.Matrix.RetryCount = 3
			}
			if inst.Matrix.RetryDelay <= 0 {
				inst.Matrix.RetryDelay = 2
			}
			// Валидация шифрования Matrix
			if inst.Matrix.Encryption && inst.Matrix.RecoveryKey == "" {
				slog.Error("Matrix encryption enabled but recovery_key is missing", "instance", inst.Name)
				inst.Matrix.Encryption = false // Отключаем, чтобы не упасть позже
			}
		}
	}

	return &cfg, nil
}

// GetInstanceByName ищет конфигурацию инстанса по его имени.
func (c *Config) GetInstanceByName(name string) *Instance {
	for i := range c.Instances {
		if c.Instances[i].Name == name {
			return &c.Instances[i]
		}
	}
	return nil
}