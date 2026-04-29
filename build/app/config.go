package main

import (
    "fmt"
    "os"
    "gopkg.in/yaml.v3"
)

// Instance содержит настройки одного экземпляра (порт, токены, разрешённые IP и т.д.)
// Поле Enabled имеет тип bool. Если в YAML приходит не булево значение (например, "yes"),
// yaml.Unmarshal вернёт ошибку, и программа завершится с сообщением о некорректном типе.
type Instance struct {
    Name       string   `yaml:"name"`
    Port       int      `yaml:"port"`
    Enabled    bool     `yaml:"enabled"`     // по умолчанию false (zero value)
    AllowedIPs []string `yaml:"allowed_ips"` // если не задано, в setDefaults подставится ["0.0.0.0/0"]
    Telegram   *struct {
        Enabled     bool   `yaml:"enabled"`
        BotToken    string `yaml:"bot_token"`
        ChatID      string `yaml:"chat_id"`
        RetryCount  int    `yaml:"retry_count"`
        RetryDelay  int    `yaml:"retry_delay"`
    } `yaml:"telegram"`
    Matrix *struct {
        Enabled     bool   `yaml:"enabled"`
        Homeserver  string `yaml:"homeserver"`
        Username    string `yaml:"username"`
        Password    string `yaml:"password"`
        AccessToken string `yaml:"access_token"`
        RoomID      string `yaml:"room_id"`
        RetryCount  int    `yaml:"retry_count"`
        RetryDelay  int    `yaml:"retry_delay"`
    } `yaml:"matrix"`
}

// Config содержит список всех экземпляров
type Config struct {
    Instances []Instance `yaml:"instances"`
}

// setDefaults заполняет пропущенные параметры значениями по умолчанию.
// Вызывается после загрузки YAML, но до проверки обязательных полей.
func setDefaults(inst *Instance) {
    // Если список разрешённых IP не задан, разрешаем все адреса (0.0.0.0/0).
    // Это соответствует поведению Python-версии, где allowed_ips был опциональным.
    if len(inst.AllowedIPs) == 0 {
        inst.AllowedIPs = []string{"0.0.0.0/0"}
    }

    // Для Telegram: если retry_count или retry_delay не заданы или <=0,
    // подставляем значения по умолчанию (3 попытки, задержка 2 секунды).
    if inst.Telegram != nil {
        if inst.Telegram.RetryCount <= 0 {
            inst.Telegram.RetryCount = 3
        }
        if inst.Telegram.RetryDelay <= 0 {
            inst.Telegram.RetryDelay = 2
        }
    }
    // Для Matrix – аналогично.
    if inst.Matrix != nil {
        if inst.Matrix.RetryCount <= 0 {
            inst.Matrix.RetryCount = 3
        }
        if inst.Matrix.RetryDelay <= 0 {
            inst.Matrix.RetryDelay = 2
        }
    }
}

// LoadConfig загружает YAML-файл, проверяет его структуру,
// устанавливает значения по умолчанию и возвращает конфигурацию.
// В случае ошибки (неверный синтаксис, отсутствие имени, небулево enabled и т.д.)
// возвращает ошибку, которую обрабатывает main.
func LoadConfig(path string) (*Config, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("read config: %w", err)
    }
    var cfg Config
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        // Сюда попадают ошибки типа: поле enabled имеет строку "yes" вместо bool,
        // или порт указан строкой, а не числом.
        return nil, fmt.Errorf("parse yaml: %w", err)
    }
    if len(cfg.Instances) == 0 {
        return nil, fmt.Errorf("no instances defined")
    }
    // Проверяем, что у каждого экземпляра есть имя.
    for i, inst := range cfg.Instances {
        if inst.Name == "" {
            return nil, fmt.Errorf("instance %d missing name", i)
        }
        setDefaults(&inst) // применяем значения по умолчанию
        cfg.Instances[i] = inst
    }
    return &cfg, nil
}