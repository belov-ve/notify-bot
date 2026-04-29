# notify-bot
notify-bot – a multi-port notifier server that accepts POST requests to the /notify endpoint with arbitrary JSON (compatible with Technitium DNS Server Failover WebHook)

---

notify-bot – многопортовый сервер-уведомитель, который принимает POST-запросы на эндпоинт /notify с произвольным JSON (совместимо с Technitium DNS Server Failover WebHook) и асинхронно отправляет сообщения в Telegram.

Поддерживает несколько независимых экземпляров (разные порты), проверку IP клиента по CIDR, повторные попытки отправки с задержкой и гибкое логирование.

---


## 📁 Структура проекта

```
project/
├── app/
│   ├── app.py
│   └── requirements.txt
├── Dockerfile
├── docker-compose.yml
├── config.yml
└── README.md
```

---



# Многопортовый уведомитель для Technitium DNS Server

**Важно:** Для точной фильтрации по IP необходимо использовать `network_mode: host` (см. docker-compose.yml).

## Быстрый старт

1. Отредактируйте `config.yml`, указав свои порты, токены и разрешённые сети.
2. Запустите:
   ```bash
   docker-compose build
   docker-compose up -d
   ```
3. Проверьте:
   ```bash
   curl http://localhost:8041/health
   curl -X POST http://localhost:8041/notify -H "Content-Type: application/json" -d '{"text": "test"}'


## Запуск без Compose

```bash
docker run -d --name notify-tbot --network host -v $(pwd)/config.yml:/app/config.yml:ro -e LOG_LEVEL=INFO notify-tbot:latest
```

## Логирование

Уровень задаётся переменной `LOG_LEVEL` (DEBUG, INFO, WARNING, ERROR).

## Лицензия

MIT
