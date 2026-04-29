# Notify-Bot

**Notify-Bot** – A multi-port notifier for Technitium DNS Server (and any JSON webhooks). Receives a POST to /notify, generates a message, and asynchronously sends it to Telegram and/or Matrix.

Supports multiple independent instances (different ports), client IP verification using CIDR, delayed retries, flexible logging, dedicated health-check port, concurrency limiting, and graceful shutdown.

---

**Notify-Bot** – Многопортовый уведомитель для Technitium DNS Server (и любых JSON webhook). Принимает POST на /notify, формирует сообщение, асинхронно отправляет в Telegram и/или Matrix.

Поддерживает несколько независимых экземпляров (разные порты), проверку IP клиента по CIDR, повторные попытки отправки с задержкой, гибкое логирование, выделенный порт для health‑check, ограничение количества одновременных отправок и корректное завершение работы.

---


## 📁 Структура проекта

```
notify-bot/
├── docker-compose.yml
├── config.yml.example
├── build/
│   ├── Dockerfile
│   └── app/
│       ├── main.go
│       ├── config.go
│       ├── handlers.go
│       ├── telegram.go
│       ├── matrix.go
│       ├── utils.go
│       ├── go.mod
│       └── go.sum
└── README.md
```

---



**Версия 2.0.0**

## Возможности

- Приём POST-запросов на `/notify` с произвольным JSON.
- Отправка уведомлений в Telegram (Bot API) и Matrix (простой HTTP API).
- Несколько независимых экземпляров на разных портах.
- Фильтрация клиентов по IP (CIDR / одиночные IP).
- Асинхронная отправка с повторными попытками (экспоненциальная задержка).
- Детальное логирование с уровнями DEBUG, INFO, WARNING, ERROR.
- Выделенный health‑check сервер – порт задаётся переменной `HEALTH_CHECK_PORT`. 
*Не проверяет разрешенные IP, логируется только в режиме DEBUG.*
- Ограничение параллельных отправок – семафор на 100 горутин. При превышении лимита отправки ожидают освобождения слота.
- Graceful shutdown – при остановке контейнера бот ждёт завершения активных отправок (до 30 секунд).

## Эндпоинты

- `GET /health` – проверка работоспособности экземпляра (если экземпляр запущен). Возвращает `{"status": "ok"}`.
- `POST /notify` – приём уведомления. Принимает JSON. Возвращает `202 Accepted` с `{"status": "accepted"}`. При ошибках – 4xx.

## Формат JSON для `/notify`

Бот ожидает JSON-объект, который может содержать **любые поля**. Все поля необязательны. Смысл полей определяется отправляющей системой (например, Technitium DNS Server).

| Поле      | Тип    | Описание                                                                 |
|-----------|--------|--------------------------------------------------------------------------|
| `text`    | string | **Основное сообщение**. Если поле присутствует, его значение становится **первой строкой** итогового текста. |
| любое другое | любой | Все остальные поля выводятся в формате `ключ: значение` (каждое с новой строки). |

**Правила формирования сообщения:**
1. Если в JSON есть поле `text`, оно добавляется первой строкой.
2. Затем выводятся все остальные поля (кроме `text`) в формате `"ключ: значение"`.
3. Если поля `text` нет, то выводятся **все** поля как `"ключ: значение"`.

### Пример для Technitium DNS Server (Failover WebHook)

Типичные поля, которые отправляет Technitium:

```json
{
  "domain": "server1.local",
  "recordType": "A",
  "healthCheck": "tcp5000",
  "status": "Failed",
  "failureReason": "Connection refused",
  "dateTime": "2026-04-27T10:16:59.7930201Z"
}
```

Сообщение, которое будет отправлено в Telegram/Matrix:

```
domain: server1.local
recordType: A
healthCheck: tcp5000
status: Failed
failureReason: Connection refused
dateTime: 2026-04-27T10:16:59.7930201Z
```

Если вы хотите добавить свой заголовок, используйте поле `text`:

```json
{
  "text": "Внимание! Проблема с DNS",
  "domain": "server1.local",
  "status": "Failed"
}
```

Результат:

```
Внимание! Проблема с DNS
domain: server1.local
status: Failed
```

## Конфигурация (config.yml)

```yaml
instances:
  - name: "telegram_bot"
    port: 8041
    enabled: true
    allowed_ips:
      - "192.168.1.0/24"
    telegram:
      enabled: true
      bot_token: "123456:ABC-DEF"
      chat_id: "-456789123"
      retry_count: 3
      retry_delay: 2


  - name: "matrix_bot"
    port: 8042
    enabled: true
    allowed_ips:
      - "0.0.0.0/0"
    matrix:
      enabled: true
      homeserver: "https://matrix.example.com"
      username: "@notify-bot:example.com"
      password: "your_strong_password"
      room_id: "!roomid:example.com"
      retry_count: 3
      retry_delay: 2


  - name: "default"
    enabled: false  # default
    port: 8050
    allowed_ips:    #  "0.0.0.0/0" default if empty
      - "0.0.0.0/0"
      - "192.168.65.1"
      - "172.17.0.1/32"
      - "10.0.0.0/8"
    telegram:
      enabled: true
      bot_token: "789101:GHI-JKL"
      chat_id: "-987654321"
      retry_count: 3  # default
      retry_delay: 2  # default
    matrix:
      enabled: true
      homeserver: "https://matrix.example.com"
      username: "@notify-bot:example.com"
      password: "secret_password"      # или access_token
      # access_token: "syt_..."        # токен вместо пароля
      room_id: "!roomid:example.com"
      retry_count: 3  # default
      retry_delay: 2  # default
```

- `allowed_ips` – опционально, по умолчанию `["0.0.0.0/0"]`.
- `retry_count` / `retry_delay` – если ≤0, подставляются 3 и 2.
- `enabled: false` – экземпляр не запускается (порт не занимается).



## Быстрый старт

1. Клонируйте репозиторий
   ```bash
   git clone https://github.com/belov-ve/notify-bot.git
   cd notify-bot
   ```

2. Создайте конфигурационный файл
Скопируйте пример и отредактируйте:
   ```bash
   cp config.yml.example config.yml
   nano config.yml
   ```

3. Соберите образ и запустите
   ```bash
   docker compose build --no-cache
   docker compose up -d
   ```

4. Проверьте, подключившись к потоку (пример для 8041/tcp)
   ```bash
   curl http://127.0.0.1:8040/health
   curl -X POST http://127.0.0.1:8041/notify -H "Content-Type: application/json" -d '{"text": "Test", "from": "curl"}'
   ```

## Сетевые режимы

- `network_mode: host` – реальный IP клиента (рекомендуется для `allowed_ips`).
- `bridge` – все клиенты видны с IP Docker-шлюза (фильтрация бесполезна). Только для тестов.

## Переменные окружения

- `LOG_LEVEL` – DEBUG, INFO, WARNING, ERROR (по умолчанию INFO).
- `HEALTH_CHECK_PORT` – порт для отдельного health‑check сервера. Если не задан – сервер не запускается.


## Пример отправки сообщений из bash-скрипта

```bash
#!/bin/bash
WEBHOOK_URL="http://localhost:8041/notify"
MESSAGE="Сервер $(hostname) перезагружен в $(date)"
curl -X POST "$WEBHOOK_URL" -H "Content-Type: application/json" -d "{\"text\": \"$MESSAGE\"}"
```


## Пример docker-compose.yml с healthcheck для запуска контейнера
>*При network_mode: host контейнер использует сеть хоста напрямую, поэтому проброс портов (ports) не требуется.*

```yaml
services:
  notify-bot:
    image: notify-bot:2.0.0
    network_mode: host
    # network_mode: bridge
    # ports:
    #   - "8040-8050:8040-8050/tcp"
    volumes:
      - ./config.yml:/app/config.yml:ro
    environment:
      - LOG_LEVEL=INFO
      - HEALTH_CHECK_PORT=8040
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8040/health"]
      interval: 30s
      timeout: 5s
      retries: 3
    restart: unless-stopped
```


## Пример запуска контейнера без Docker Compose
> *healthcheck в примере не используется.*

```bash
docker build -t notify-bot:2.0.0 .
docker run -d --name notify-bot --network host -v $(pwd)/config.yml:/app/config.yml:ro -e LOG_LEVEL=INFO notify-bot:2.0.0
```

## Пример интеграции с `Technitium DNS Server` 

### Конфигурация APP Failover
```config
{
  "healthChecks": [
    {
      "name": "wwwServer",
      "type": "http",
      "interval": 30,
      "retries": 2,
      "timeout": 5,
      "url": "http://check-host:81/api/health",
      "webHook": "notify-bot"
    },
    {
      "name": "tcp5000",
      "type": "tcp",
      "interval": 30,
      "retries": 2,
      "timeout": 5,
      "port": 5000,
      "webHook": "notify-bot"
    }
  ],
  "webHooks": [
  {
    "name": "notify-bot",
    "enabled": true,
    "urls": [
      "http://<fqdn or ip notify-bot>:<port>/notify"
    ]
  }
  ]
}
```

### Запись зоны
```dns zone
# App Name: Failover
# Class Path: Failover.Address
# Record Data:
{
  "primary": [
    "192.168.1.12"
  ],
  "secondary": [
    "192.168.1.13"
  ],
  "serverDown": [
    null
  ],
  "healthCheck": "wwwServer",
  "healthCheckUrl": null,
  "allowTxtStatus": false
}

# App Name: Failover
# Class Path: Failover.CNAME
# Record Data:
{
  "primary": "mainhost.local",
  "secondary": ["reserve1.local","reserve2.local"],
  "serverDown": null,
  "healthCheck": "tcp5000",
  "healthCheckUrl": null,
  "allowTxtStatus": false
}
```

## Лицензия

MIT
