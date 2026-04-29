# Notify-Bot

`Notify-Bot` – A multi-port notifier for Technitium DNS Server (and any JSON webhooks).
Receives a POST to /notify, generates a message, and asynchronously sends it to Telegram and/or Matrix.

Supports multiple independent instances (different ports), client IP verification using CIDR, delayed retries, and flexible logging.

---

`Notify-Bot` – Многопортовый уведомитель для Technitium DNS Server (и любых JSON webhook).
Принимает POST на /notify, формирует сообщение, асинхронно отправляет в Telegram и/или Matrix.

Поддерживает несколько независимых экземпляров (разные порты), проверку IP клиента по CIDR, повторные попытки отправки с задержкой и гибкое логирование.

---


## 📁 Структура проекта

```
notify-bot/
├── app/
│   ├── app.py
│   ├── matrix_sender.py
│   └── requirements.txt
├── Dockerfile
├── docker-compose.yml
├── config.yml.example
└── README.md
```

---



**Версия 1.1.0**

## Возможности

- Приём POST-запросов на `/notify` с произвольным JSON.
- Отправка уведомлений в Telegram (Bot API) и Matrix (через простой HTTP API).
- Поддержка нескольких независимых экземпляров на разных портах.
- Фильтрация клиентов по IP (CIDR) – работает только в режиме `network_mode: host`.
- Асинхронная отправка с повторными попытками (экспоненциальная задержка).
- Детальное логирование (stdout/stderr) с уровнями DEBUG/INFO/WARNING/ERROR.

## Эндпоинты

- `GET /health` – проверка работоспособности экземпляра. Возвращает `{"status": "ok"}`.
- `POST /notify` – приём уведомления. Ожидает JSON. Возвращает `{"status": "accepted"}` (HTTP 202) после асинхронной отправки. В случае ошибки (неверный IP, отсутствие JSON) возвращает 4xx.

## Быстрый старт

### 1. Клонируйте репозиторий

```bash
git clone https://github.com/belov-ve/notify-bot.git
cd notify-bot

# Переключиться на необходимый bench, к примеру 1.1.0
git checkout 1.1.0

```

### 2. Создайте конфигурационный файл

Скопируйте пример и отредактируйте:

```bash
cp config.yml.example config.yml
nano config.yml
```

### 3. Соберите образ и запустите

```bash
docker compose build --no-cache
docker compose up -d
```

### 4. Проверьте, подключившись к потоку (пример для 8041/tcp)

```bash
curl http://localhost:8041/health
curl -X POST http://localhost:8041/notify -H "Content-Type: application/json" -d '{"text": "Hello"}'
```

## Пример отправки сообщений из bash-скрипта

```bash
#!/bin/bash
WEBHOOK_URL="http://localhost:8041/notify"
MESSAGE="Сервер $(hostname) перезагружен в $(date)"
curl -X POST "$WEBHOOK_URL" -H "Content-Type: application/json" -d "{\"text\": \"$MESSAGE\"}"
```

## Пример docker-compose для запуска контейнера (без сборки)
```yaml
services:
  notify-bot:
    image: notify-bot:1.1.0
    network_mode: host
    # network_mode: bridge
    # ports:
    #   - "8040-8050:8040-8050/tcp"
    volumes:
      - ./config.yml:/app/config.yml:ro
    environment:
      - LOG_LEVEL=${LOG_LEVEL:-INFO}
    restart: unless-stopped
```

## Пример запуска контейнера без Docker Compose

```bash
docker build -t notify-bot:1.1.0 .
docker run -d --name notify-bot --network host -v $(pwd)/config.yml:/app/config.yml:ro -e LOG_LEVEL=INFO notify-bot:1.1.0
```

## Сетевые режимы

- `host` – реальный IP клиента (рекомендуется для `allowed_ips`)
- `bridge` – все клиенты видны с IP Docker-шлюза (фильтрация бесполезна)

## Переменные окружения

- `LOG_LEVEL` – DEBUG, INFO, WARNING, ERROR (по умолчанию INFO)

## Лицензия

MIT
