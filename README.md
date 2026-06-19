# Notify-Bot

**Notify-Bot** – A multi-port notifier for Technitium DNS Server (and any JSON webhooks). Receives a POST to /notify, saves it to a persistent SQLite queue, and reliably delivers it to Telegram and/or Matrix.

Supports multiple independent instances, client IP verification, **Guaranteed Delivery (Outbox pattern)**, TTL for messages, dynamic configuration reloading, queue statistics, and graceful shutdown.

---

**Notify-Bot** – Многопортовый уведомитель для Technitium DNS Server (и любых JSON webhook). Принимает POST на /notify, сохраняет в отказоустойчивую очередь SQLite и гарантированно доставляет в Telegram и/или Matrix.

Поддерживает несколько независимых экземпляров, проверку IP клиента, сквозное шифрование (E2EE) для Matrix, гарантированную доставку с учетом времи жизни сообщений (TTL), динамическую перезагрузку конфигурации, статистику очередей и корректное завершение работы.

---

## 📁 Структура проекта
```
notify-bot/
├── docker-compose.yml
├── config.yml.example
├── build/
│   ├── Dockerfile
│   └── app/
│       ├── main.go          # Точка входа, инициализация и динамический релоад
│       ├── config.go        # Загрузка и валидация конфигурации (YAML)
│       ├── handlers.go      # HTTP обработчики (/notify, /health, /stats)
│       ├── database.go      # Уровень хранения SQLite и логика Outbox
│       ├── worker.go        # Фоновый процесс доставки (FIFO, TTL, ретраи)
│       ├── manager.go       # Динамическое управление серверами инстансов
│       ├── telegram.go      # Интеграция с Telegram Bot API
│       ├── matrix.go        # Интеграция с Matrix (поддержка E2EE)
│       ├── utils.go         # Вспомогательные функции (Ordered JSON, IP)
│       ├── go.mod           # Зависимости проекта
│       └── go.sum           # Контрольные суммы зависимостей
└── README.md
```

---

**Версия 3.3.0**

## Возможности
- **Интерактивные меню и выполнение скриптов (Matrix и Telegram)**: Интерактивные меню команд (вызов по `/menu` в Telegram и `!menu` в Matrix) с поддержкой быстрого запуска по эмодзи-реакциям (`m.reaction` в Matrix комнаты) или с помощью Inline-кнопок (в Telegram). Бот поддерживает выполнение внешних HTTP-запросов и запуск локальных скриптов внутри контейнера с настраиваемым таймаутом `SCRIPT_TIMEOUT`.
- **Красивое форматирование и обработка ответов команд**: Бот автоматически распознает и красиво форматирует JSON и XML ответы от выполненных команд. При форматировании JSON-объектов сохраняется исходный порядок полей (через `DecodeOrderedJSON`), а текстовое поле `text` (при наличии на верхнем уровне) всегда выводится первым без префикса ключа.
- **Безопасное логирование и маскирование**: При сбоях выполнения команд чаты защищены от утечек технических деталей (секреты, пути, stderr скриптов) — в чат высылается строго одно сообщение `❌ Ошибка при выполнении команды /<имя_команды>` (или `!<имя_команды>`). Полные логи пишутся в stderr бота с автоматическим маскированием паролей и URL-параметров через `maskCredentialsInText`.
- **Парсинг метаданных в multipart/form-data**: Бот полностью извлекает все текстовые поля формы, переданные при multipart-отправке файлов, и собирает их в единое упорядоченное описание события с сохранением порядка полей, аналогично JSON-запросам.
- **Гарантированная доставка**: Все сообщения сохраняются в SQLite перед отправкой.
- **Отложенная доставка**: Сообщения, доставленные не с первой попытки, помечаются меткой `[Отложенная доставка]` с временем оригинала и часовым поясом.
- **TTL (Time To Live)**: Настраиваемое время жизни сообщения в очереди.
- **Мгновенная реакция (Fast Sync)**: Отправка начинается сразу после приема запроса.
- **Приоритеты**: Новые сообщения обрабатываются первыми, повторные попытки следуют за ними (FIFO).
- **Сквозное шифрование (Matrix E2EE)**: Поддержка зашифрованных комнат Matrix с хранением состояния в SQLite.
- **Сохранение порядка JSON**: Бот сохраняет оригинальный порядок полей из входящего JSON-запроса в тексте уведомления.
- **Метки времени (ShowTime)**: Опциональное добавление времени получения сообщения к тексту уведомления.
- **Динамический конфиг (Zero-Downtime)**: Перечитывание настроек без прерывания соединений. Рестарт порта только при смене номера `port`.
- **Блокировка отправки**: Параметр `block_delivery` позволяет временно приостановить отправку (сообщения копятся в очереди).
- **Статистика**: Эндпоинт `/stats` возвращает информацию о состоянии всех очередей в JSON.
- **Логирование**: Детальное логирование с уровнями DEBUG, INFO, WARNING, ERROR.
- **Сетевые фильтры**: Проверка IP клиента по CIDR.
- **Graceful shutdown**: Бот дожидается завершения активных процессов перед выходом.


## Эндпоинты
- `GET /health` – проверка работоспособности (на глобальном порту мониторинга `HEALTH_CHECK_PORT`, по умолчанию `8040`). 
  * Ответ: `{"status": "ok", "version": "3.3.0"}`
- `GET /stats` – статистика очередей (на глобальном порту мониторинга `HEALTH_CHECK_PORT`, по умолчанию `8040`).
  * Ответ: `{"instance_1": 0, "instance_2": 5}`
- `POST /notify` – приём уведомления (на портах инстансов). Маршрутизация запросов происходит динамически по заголовку `Content-Type`.
  * Ответ: `{"status": "accepted", "reqID": "726800"}`

## Формат запросов для `/notify`

### 1. Формат JSON (`application/json`)
Бот ожидает JSON-объект, который может содержать **любые поля**. Все поля необязательны. Порядок полей в итоговом сообщении соответствует порядку во входящем JSON.

Служебные поля:
* `text` (string): **Основное сообщение**. Если поле присутствует, его значение становится **первой строкой** текста уведомления.
* `image` (string): **Изображение в формате Base64** (поддерживается как обычный Base64, так и Data URI, например: `data:image/png;base64,...`). Картинка автоматически декодируется, отправляется в Telegram как фото и в Matrix как зашифрованное вложение.
  * *Резервный режим (заглушка)*: Если при декодировании Base64 возникает ошибка, бот автоматически генерирует серую PNG-картинку-заглушку `300x100` с предупреждением «WARNING: Base64 decode failed!» и красным крестом, чтобы отправка текстовой части уведомления гарантированно не прерывалась.

Все остальные поля выводятся в формате `ключ: значение` (каждое с новой строки).

| Поле      | Тип    | Описание                                                                 |
|-----------|--------|--------------------------------------------------------------------------|
| `text`    | string | Основное текстовое описание. Выводится первой строкой. |
| `image`   | string | Изображение, закодированное в Base64. |
| любое другое | любой | Все остальные поля выводятся в формате `ключ: значение` в оригинальном порядке. |

**Правила формирования сообщения:**
1. Если в JSON есть поле `text`, оно добавляется первой строкой.
2. Затем выводятся все остальные поля (кроме `text` и `image`) в формате `"ключ: значение"`, **строго сохраняя оригинальный порядок** из JSON-запроса.
3. Если поля `text` нет, то выводятся **все** поля (кроме `image`) как `"ключ: значение"` в оригинальном порядке.

### 2. Формат Multipart (`multipart/form-data`)
Используется для прямой отправки любых файлов, документов или изображений без необходимости предварительного кодирования в Base64.

**Заголовки запроса (HTTP Headers):**
* `Content-Type: multipart/form-data; boundary=<граничный_символ>`

**Передаваемые поля (Form Fields):**
* `file` (бинарные данные) — прикрепляемый файл (изображение или документ). В заголовке `Content-Disposition` для этой части формы обязательно должен быть указан параметр `filename="..."`.
* `text` (строка, опционально) — текстовый комментарий (подпись) к файлу. Если присутствует, выводится первой строкой.
* Любые другие текстовые поля (строка, опционально) — произвольные дополнительные параметры. Бот автоматически извлекает их и добавляет к описанию в формате `ключ: значение`.

#### Примеры отправки:

**Через консоль с помощью `curl`:**
Отправка файла `logs.txt` с подписью на инстанс (например, порт `8043`):
```bash
curl -F "file=@logs.txt" -F "text=Лог работы приложения" http://127.0.0.1:8043/notify
```

Бот автоматически проанализирует первые 512 байт файла для определения его MIME-типа:
- Если файл распознан как изображение (например, `image/png`, `image/jpeg`), в Telegram он уйдет как встроенное фото (`sendPhoto`).
- Для остальных типов (текстовые логи, архивы, PDF и т.д.) он будет отправлен как вложенный документ (`sendDocument`).
- В зашифрованных комнатах Matrix E2EE файл будет предварительно зашифрован на клиенте бота, загружен на homeserver и отобразится в чате как картинка (`m.image`) или файл (`m.file`).

### Логика хранения и очистки файлов (Media)
* Все входящие файлы временно сохраняются в каталоге `/app/data/media/`.
* **Счетчик ссылок (Reference Counting)**: Файл удаляется с диска только после того, как он был успешно доставлен во все каналы (например, и в Telegram, и в Matrix). Это предотвращает преждевременное удаление при многоканальной рассылке.
* **Автоочистка (Orphan Cleanup)**: На старте бота и каждые 12 часов запускается процесс удаления неиспользуемых файлов, у которых дата модификации старше 10 минут (защитный интервал от удаления файлов в момент их загрузки).

### Пример для Technitium DNS Server

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

Сообщение в Telegram/Matrix:
```
domain: server1.local
recordType: A
healthCheck: tcp5000
status: Failed
failureReason: Connection refused
dateTime: 2026-04-27T10:16:59.7930201Z
```


### Пример сообщения с произвольным заголовком в поле `text`:

```json
{
  "text": "Внимание! Проблема с DNS",
  "domain": "server1.local",
  "status": "Failed"
}
```


## Конфигурация (config.yml)
```yaml
instances:
  - name: "telegram_bot"
    port: 8041
    enabled: true
    ttl: 3600
    show_time: true       # Добавлять метку времени к сообщению (default: false)
    block_delivery: false # Временная блокировка отправки (сообщения копятся в очереди)
    allowed_ips:
      - "192.168.1.0/24"
    telegram:
      enabled: true
      bot_token: "123456:ABC-DEF"
      chat_id: "-456789123"
      menu: "camera_menu"    # ID подключенного меню для Telegram


  - name: "matrix_bot"
    port: 8042
    enabled: true
    ttl: 86400
    show_time: false
    block_delivery: false   # Временная блокировка отправки (true - сообщения копятся в очереди).
    allowed_ips:
      - "0.0.0.0/0"
    matrix:
      enabled: true
      homeserver: "https://matrix.example.com"
      username: "@notify-bot:example.com"
      password: "your_strong_password"   # или access_token
      encryption: true                   # Включить сквозное шифрование (E2EE)
      recovery_key: "ваша_фраза_восстановления" # Требуется для E2EE
      menu: "camera_menu"                # ID подключенного меню. Наличие меню активирует прослушивание чата
      room_id: "!roomid:example.com"


  - name: "default"
    enabled: false      # default
    port: 8050
    ttl: 0              # default
    show_time: false    # default
    block_delivery: false   # default: Временная блокировка отправки (true - сообщения копятся в очереди).
    allowed_ips:        # "0.0.0.0/0" по умолчанию, если пусто
      - "0.0.0.0/0"
      # - "172.17.0.1/32"
      # - "10.0.0.0/8"
    telegram:
      enabled: true
      bot_token: "789101:GHI-JKL"
      chat_id: "-987654321"
      menu: "camera_menu"    # ID подключенного меню для Telegram
      retry_count: 3    # default
      retry_delay: 2    # default
    matrix:
      enabled: true
      homeserver: "https://matrix.example.com"
      username: "@notify-bot:example.com"
      password: "secret_password"       # или access_token
      # access_token: "syt_..."         # токен вместо пароля
      encryption: false # default
      room_id: "!roomid:example.com"
      retry_count: 3    # default
      retry_delay: 2    # default

# Глобальная секция меню для интерактивных команд
menus:
  - id: "camera_menu"
    items:
      - name: "snapshot/hd"
        url: "http://localhost:8090/snapshot/hd?send=true"
        description: "Получить HD изображение с камеры"
        reaction: "📸"
      - name: "snapshot/sd"
        url: "http://localhost:8090/snapshot/sd?send=true"
        description: "Получить SD изображение"
        reaction: "📷"
      - name: "status"
        script: "stats.sh"
        description: "Посмотреть статистику очередей"
        reaction: "📊"
```

- `allowed_ips` – опционально, по умолчанию `["0.0.0.0/0"]`.
- `retry_count` / `retry_delay` – если ≤0, подставляются 3 и 2.
- `enabled: false` – экземпляр не запускается (порт не занимается).
- `ttl`: - время жизни сообщения в очереди (сек). По умолчанию 0 (одна попытка), гарантированная доставка отключена.
- `block_delivery` - по умолчанию false. Временная блокировка отправки (true - сообщения копятся).
- `show_time` - добавлять метку времени приема сообщения (YYYY-MM-DD HH:MM:SS MST) в конец текста.
- `encryption` - активация сквозного шифрования для Matrix.
- `recovery_key` - ключ восстановления Matrix (Recovery Key), необходимый для инициализации E2EE сессии.
- `menu` - уникальный идентификатор подключенного меню интерактивных команд (для Matrix и Telegram). Наличие меню активирует фоновое прослушивание сообщений и реакций (в Matrix) или лонг-поллинг (в Telegram).
- `script` - (опционально) имя локального файла скрипта в директории `/app/scripts/` (например, `stats.sh` или `cub-ip.sh`). Если в элементе меню настроен `script`, он имеет приоритет перед `url`. Скрипты должны возвращать вывод в формате JSON. Таймаут выполнения скрипта настраивается через переменную окружения `SCRIPT_TIMEOUT` в `docker-compose.yml` (по умолчанию 15 секунд). При сбоях выполнения в чат выводится фиксированная строка `❌ Ошибка при выполнении команды !<имя_команды>`, а детальные ошибки пишутся только в логи бота.


## Интерактивное меню команд бота (Matrix и Telegram)

Бот поддерживает интерактивное меню команд, позволяющее выполнять внешние HTTP GET-запросы или локальные bash-скрипты, и возвращать результат прямо в чат.

### 1. Активация меню в конфигурации
Для активации интерактивного меню необходимо:
1. Описать глобальные меню в разделе `menus` конфигурационного файла.
2. Привязать меню к инстансу через параметр `menu` в соответствующем блоке `telegram` или `matrix`.

Пример:
```yaml
telegram:
  enabled: true
  bot_token: "..."
  chat_id: "..."
  menu: "my_menu" # Подключение меню
```

Наличие параметра `menu` автоматически запускает прослушивание входящих сообщений:
- Для **Telegram** запускается цикл лонг-поллинга (`getUpdates` с таймаутом `30` секунд). При старте бота автоматически сбрасываются все накопившиеся за время офлайна команды (`offset=-1&limit=1`), предотвращая лавину ложных запусков.
- Для **Matrix** запускается фоновый цикл синхронизации (`SyncRequest`).

### 2. Ограничение доступа (Access Control)
В целях безопасности бот обрабатывает текстовые сообщения и нажатия Inline-кнопок **строго** от того чата / пользователя, чей идентификатор (`chat_id` для Telegram или `room_id` для Matrix) указан в конфигурации этого инстанса. Сообщения из других чатов или от других пользователей игнорируются.

### 3. Вызов меню и синтаксис команд
Пользователь может запросить вывод меню с доступными командами:
- В **Telegram**: отправив текстовую команду `/menu` (а также поддерживаются `!menu` и `!nemu`). Бот присылает сообщение со списком команд и встроенной клавиатурой Inline Keyboard (Inline-кнопками). При нажатии на кнопку бот отправляет `callback_query` с данными `cmd:<имя_команды>`, мгновенно подтверждает нажатие (вызовом `answerCallbackQuery` для скрытия значка загрузки на кнопке) и асинхронно запускает выполнение команды.
- В **Matrix**: отправив команду `!menu` (или `!nemu`), бот пришлет текстовый список команд.

Вызов конкретной команды напрямую:
- В **Telegram** поддерживаются префиксы `/` и `!`. Поскольку Telegram считает слэши разделителями и не делает кликабельными ссылки с внутренними слэшами (например, `/snapshot/hd`), бот поддерживает автоматическую замену слэшей на подчеркивание:
  - Имя команды в меню: `snapshot/hd`
  - Inline-кнопка в Telegram отправит `snapshot_hd`
  - Пользователь в Telegram может ввести: `/snapshot_hd`, `!snapshot_hd`, `/snapshot/hd` или `!snapshot/hd` — бот корректно сопоставит это с командой `snapshot/hd` из конфигурации.
- В **Matrix** поддерживается префикс `!` (например, `!snapshot/hd`). Также в Matrix поддерживаются **эмодзи-реакции**: при добавлении настроенного эмодзи (например, 📸) на любое сообщение в чате, бот автоматически выполнит связанную команду. Для надёжного сравнения бот нормализует эмодзи, вырезая вариативные селекторы `U+FE0F` и `U+FE0E`.

### 4. Выполнение команд
Команды могут быть двух типов:
1. **HTTP-запрос (`url`)**: Бот выполняет HTTP GET запрос по указанному URL с таймаутом `15` секунд.
2. **Локальный скрипт (`script`)**: Бот запускает исполняемый файл из каталога `/app/scripts/` (скрипт должен иметь права на исполнение). Время выполнения скрипта ограничено переменной окружения `SCRIPT_TIMEOUT` в `docker-compose.yml` (по умолчанию `15` секунд).

Все команды выполняются **асинхронно в отдельных горутинах**, что предотвращает блокировку циклов прослушивания и синхронизации.

### 5. Обработка ответов
Бот обрабатывает полученный от команды ответ (до 1 МБ) следующим образом:
- **JSON / XML**: Бот пытается распарсить ответ. Если это JSON, парсинг сохраняет исходный порядок полей (через `DecodeOrderedJSON`). Поле `text` на верхнем уровне (если есть) всегда выводится первой строкой без названия ключа. Остальные поля выводятся в виде красивого списка `ключ: значение`. Если это XML, автоматически отсекается корневой тег-контейнер. HTML-сущности в тексте ответа автоматически декодируются перед отправкой в Telegram.
- **Бинарный ответ (Изображение / Документ)**: Если заголовок ответа (`Content-Type`) или первые 512 байт тела ответа распознаются как изображение (MIME-тип `image/*`), бот отправляет файл в чат как фото. Все остальные типы файлов отправляются как вложенные документы.
- **Текст / Пустой ответ**: Простой текст отправляется как есть. Если ответ пустой, бот присылает сообщение об успешном выполнении команды.

### 6. Безопасность при ошибках
При возникновении ошибок выполнения (таймаут, сетевой сбой, ошибка скрипта `exit status > 0` и т.д.):
- В чат высылается строго стандартное сообщение без технических подробностей: `❌ Ошибка при выполнении команды /<имя_команды>` (или `!<имя_команды>`). Это защищает от утечки путей, токенов, структуры базы данных и прочих приватных данных.
- Полная техническая информация (включая `stderr` скрипта) логируется в `stderr` бота с уровнем `ERROR`.
- Перед выводом в лог все конфиденциальные данные (пароли, access-токены, URL-параметры) маскируются с помощью функции `maskCredentialsInText`.


## Настройка Matrix и верификация (E2EE)

Если вы включили `encryption: true`, сообщения будут зашифрованы. Для корректной работы бота необходимо пройти процесс верификации сессии (Cross-signing) один раз.

### Процесс верификации (SAS Emoji):
1. **Запуск**: После старта бота найдите в логах строку `Matrix device identity`. Там будет указан `device_id` и `fingerprint` (отпечаток ключа).
2. **Инициация**: Откройте Element, перейдите в **Настройки -> Безопасность и приватность -> Сессии (Devices)**. Найдите сессию вашего бота (например, `notify-bot-XXXX`).
3. **Запрос**: Нажмите на сессию и выберите **Верифицировать (Verify)**. В появившемся окне выберите **Верифицировать с помощью эмодзи (Verify with Emoji)**.
4. **Сравнение**:
   - В логах бота появится раздел `--- MATRIX VERIFICATION EMOJIS ---`.
   - Сравните набор эмодзи и цифр в логах бота с тем, что отображается в Element.
5. **Подтверждение**:
   - Бот выждет 3 секунды и автоматически отправит подтверждение (Confirm) со своей стороны.
   - Вам нужно нажать **«Они совпадают» (They match)** в приложении Element.
6. **Результат**: Статус сессии в Element станет «Верифицировано» (зеленый щит). Теперь бот — доверенное устройство.

> [!TIP]
> При удалении файла базы данных `<account_id>.db` (в папке `data/`) сессия сбрасывается, и верификацию нужно будет пройти повторно.


## Быстрый старт

1. Клонируйте репозиторий
   ```bash
   git clone -b main https://github.com/belov-ve/notify-bot.git
   cd notify-bot
   ```

2. Создайте конфигурационный файл и каталог для хранения данных
Скопируйте пример и отредактируйте:
   ```bash
   mkdir -p data
   cp config.yml.example config.yml
   nano config.yml
   ```

3. Соберите образ и запустите
   ```bash
   docker compose up -d --build --no-cache
   ```

4. Проверьте, подключившись к потоку (пример для 8041/tcp)
   ```bash
   curl http://127.0.0.1:8040/health
   curl -X POST http://127.0.0.1:8041/notify -H "Content-Type: application/json" -d '{"text": "Test", "from": "curl"}'
   ```


## Пример docker-compose.yml
```yaml
services:
  notify-bot:
    container_name: notify-bot
    build:
      context: ./build
    image: notify-bot:3.3.0
    network_mode: host
    # network_mode: bridge
    # ports:
    #   - "8040:8040/tcp"
    #   - "8041-8050:8041-8050/tcp"
    volumes:
      - ./config.yml:/app/config.yml:ro
      - ./data:/app/data:rw
    environment:
      - LOG_LEVEL=${LOG_LEVEL:-INFO}
      - HEALTH_CHECK_PORT=8040
      - DB_PATH=/app/data/notify_bot.db
      - TZ=${TZ:-UTC}
      # - TZ=Europe/Moscow
    restart: unless-stopped
    stop_grace_period: 20s
```

### Сетевые режимы

- `network_mode: host` – реальный IP клиента (рекомендуется для `allowed_ips`).
- `bridge` – все клиенты видны с IP Docker-шлюза (фильтрация по IP может не работать корректно без проксирования заголовков).

### Настройка часового пояса

По умолчанию бот использует время UTC. Чтобы сообщения и логи отображали местное время, задайте переменную `TZ` в `docker-compose.yml`:
```yaml
    environment:
      - TZ=Europe/Moscow
```

### Переменные окружения

- `LOG_LEVEL` – Уровень детализации логов: `DEBUG`, `INFO`, `WARNING`, `ERROR` (по умолчанию `INFO`).
- `HEALTH_CHECK_PORT` – Порт для глобального HTTP-сервера мониторинга (эндпоинты `/health` и `/stats`). Если не задан, сервер мониторинга не запускается.
- `DB_PATH` – Абсолютный путь к файлу базы данных SQLite в контейнере (по умолчанию `/app/data/notify_bot.db`).
- `CONFIG_PATH` – Альтернативный путь к конфигурационному файлу `config.yml` (по умолчанию `/app/config.yml` или `./config.yml`).
- `RETRY_INTERVAL` – Интервал между повторными попытками отправки отложенных сообщений в секундах (по умолчанию `60` секунд).
- `SCRIPT_TIMEOUT` – Максимальный лимит времени выполнения локальных скриптов меню в секундах (по умолчанию `15` секунд).
- `TZ` – Системный часовой пояс (например, `Europe/Moscow`), используемый для корректного отображения локального времени в сообщениях и логах.


## Использование healthcheck для восстановление контейнеров (`Autoheal`)
По умолчанию Docker Engine умеет проверять состояние бота (Healthcheck) и помечать зависший контейнер статусом unhealthy. Однако сам Docker не перезагружает такие контейнеры — директива restart: unless-stopped спасает только в случае полного падения главного процесса.

Чтобы бот (и другие сервисы) автоматически восстанавливал работу при зависаниях сети или внутренних процессах, рекомендуется использовать легковесный служебный контейнер — Autoheal. Он непрерывно мониторит статусы в Docker и принудительно перезагружает «больные» контейнеры.

Запустить `Autoheal` можно отдельным файлом docker-compose.yml или добавить в виде службы в существующий:
```yaml
services:
  autoheal:
    image: willfarrell/autoheal
    container_name: autoheal
    restart: always
    network_mode: none
    environment:
      # Режим 1: Глобальный. Autoheal будет перезагружать любой зависший контейнер на сервере
      - AUTOHEAL_CONTAINER_LABEL=all 
      # Режим 2: Точечный. Следим только за избранными сервисами
      # - AUTOHEAL_CONTAINER_LABEL=autoheal 
    volumes:
      - /etc/localtime:/etc/localtime:ro
      - /var/run/docker.sock:/var/run/docker.sock
    # labels:
    #   - "autoheal=true"
```

### Как настроить точечный мониторинг?
Перезагружать все контейнеры подряд (режим all) может быть небезопасно для сложных систем (например, баз данных).

Рекомендуется использовать точечный мониторинг. Для этого раскомментируйте строку AUTOHEAL_CONTAINER_LABEL=autoheal в настройках выше, а в конфигурацию самого notify-bot добавьте специальную метку (она уже подготовлена в примере):

```yaml
# Указываем Autoheal, что этот контейнер нужно спасать при зависании
    labels:
      - "autoheal=true"
```


### Пример запуска контейнера без Docker Compose
```bash
docker run -d \
  --name notify-bot \
  --network host \
  -v $(pwd)/config.yml:/app/config.yml:ro \
  -v $(pwd)/data:/app/data:rw \
  -e TZ=Europe/Moscow \
  notify-bot:3.3.0
```


### Пример отправки из bash-скрипта

```bash
#!/bin/bash
WEBHOOK_URL="http://localhost:8041/notify"
MESSAGE="Сервер $(hostname) перезагружен в $(date)"
curl -X POST "$WEBHOOK_URL" -H "Content-Type: application/json" -d "{\"text\": \"$MESSAGE\"}"
```

---

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

---

## Пример интеграции c `netwatch Mikrotik` (RouterOS 7.18+) 

### Шаг 1: Добавить скрипт отправки уведомлений

1. Заменить значение переменных на актуальные:
	- bot - fqdn or ip notify-bot
	- port - порт конфигурации
1. Присвоить имя добавленному скрипту: notify
1. Установить скрипту право `Don't Require Permissions`

```
# notify – уведомление для одного или нескольких Netwatch

:local bot "notify-bot.local"
:local port "8041"
#============================

:log info "notify: name=$name, host=$host, status=$status"
:local readableName "Not defined"
:local checkIP      "Not defined"
:local eventStatus  "Not defined"

# Читаемое имя (поле Name в Netwatch)
:if ([:typeof $name] = "str" and $name != "") do={ :set readableName $name }

# IP-адрес проверяемого хоста (переменная $host от Netwatch)
:if ([:typeof $host] = "str" and $host != "") do={ :set checkIP $host }

# Состояние (up/down), передаваемое Netwatch
:if ([:typeof $status] = "str") do={ 
    :if ($status = "up") do={
        :set eventStatus "🟢 $status"
    } else={
        :set eventStatus "🔴 $status"
    }
}


/tool fetch url="http://$bot:$port/notify" http-method=post \
    http-data="{\"text\":\"Mikrotik\",\"name\":\"$readableName\",\"check\":\"$checkIP\",\"status\":\"$eventStatus\"}"
```

### Шаг 2: Добавить контролируемый ресурс
Для примера используется тип проверки `icmp` и `host` 192.168.1.2 
```
/tool netwatch add host=192.168.1.2 name="Check Router" type=simple interval=30s timeout=10s up-script=notify down-script=notify ignore-initial-up=yes
```

---

## Пример интеграции с `Zabbix 7` 

### Шаг 1: Создание Webhook-медиа (Способа оповещения)
1. В Оповещения (Alerts) → Способы оповещений (Media types).
2. Создать способ оповещений (Create media type).
3. Имя (Name): задайте понятное имя, например Notify-Bot.
4. Тип (Type): из выпадающего списка выберите `Webhook`.

Параметры (Parameters):

- EventNSeverity: {EVENT.NSEVERITY}
- Message: {ALERT.MESSAGE}
	{ALERT.MESSAGE} подхватит текст, который вы напишете в шаблоне сообщений (на вкладке Message templates). Именно он и будет отправлен через сервис Notify-Bot.
- Severity: {EVENT.SEVERITY}
- Subject: {ALERT.SUBJECT}
- URL: ваш адрес бота, например http://\<IP Notify-Bot>:\<Port>/notify
- Скрипт (Script):
```javascript
try {
    var params = JSON.parse(value);

    // --- Функция выбора символа по уровню важности ---
    function getSeverityEmoji(severityName, nseverity) {
        // Приоритет: числовой уровень из {EVENT.NSEVERITY} (0-5)
        if (nseverity !== undefined && nseverity !== '') {
            var numericSeverity = parseInt(nseverity);
            if (numericSeverity === 5) return '❌';   // Disaster
            if (numericSeverity === 4) return '🔴';   // High
            if (numericSeverity === 3) return '🟠';   // Average
            if (numericSeverity === 2) return '🟡';   // Warning
            if (numericSeverity === 1) return '🔵';   // Information
            if (numericSeverity === 0) return '⚪';   // Not classified
        }

        // Резерв: по названию (на случай, если число не пришло)
        var s = String(severityName).toLowerCase();
        if (s === 'disaster') return '❌';
        if (s === 'high') return '🔴';
        if (s === 'average') return '🟠';
        if (s === 'warning') return '🟡';
        if (s === 'information') return '🔵';
        return '⚪';
    }

    var severityName = params.Severity || '';
    var nseverity = params.EventNSeverity;
    var emoji = getSeverityEmoji(severityName, nseverity);

    // --- Формируем сообщение из непустых частей ---
    var parts = [];
    if (params.Subject) parts.push(params.Subject);
    if (severityName && severityName.trim() !== '') {
        parts.push(emoji + ' ' + severityName);
    }
    if (params.Message) parts.push(params.Message);
    var fullMessage = parts.join('\n');

    // --- Отправка ---
    var request = new HttpRequest();
    request.addHeader('Content-Type: application/json');
    var payload = { "text": fullMessage };
    var response = request.post(params.URL, JSON.stringify(payload));

    if (request.getStatus() < 200 || request.getStatus() >= 300) {
        throw 'Request failed with status code: ' + request.getStatus();
    }
    return 'OK';
} catch (error) {
    Zabbix.Log(4, '[Webhook] ERROR: ' + error);
    throw 'Sending failed: ' + error;
}
```

5. На вкладке "Шаблоны сообщений (Message templates)"
- Добавить (Add).
- Тип сообщения (Message type): выберите "Problem".
Оставить значение по умолчанию или написать свой вариант, например:
	- Subject: 🟥 Problem: {EVENT.NAME}
	- Сообщение (Message): 
	```
	Host: {HOST.NAME}
	Problem started at {EVENT.TIME} on {EVENT.DATE}
	Operational data: {EVENT.OPDATA}
	Original problem ID: {EVENT.ID}
	{TRIGGER.URL}
	```
- Нажмите Добавить (Add).
- Тип сообщения "Сообщение о восстановлении" (Problem recovery).
	- Subject: 🟩 Resolved in {EVENT.DURATION}: {EVENT.NAME}
	- Сообщение (Message):
	```
	Host: {HOST.NAME}
	Problem has been resolved at {EVENT.RECOVERY.TIME} on {EVENT.RECOVERY.DATE}
	Problem duration: {EVENT.DURATION}
	Original problem ID: {EVENT.ID}
	{TRIGGER.URL}
	```
- Нажмите Добавить (Add).
- Можно добавить сообщение обновления проблемы. Нажмите Добавить (Add).
- Тип сообщения "Обновления проблемы." (Problem update).
	- Subject: 🟨 Updated problem in {EVENT.AGE}: {EVENT.NAME}
	- Сообщение (Message): 
	```
	{USER.FULLNAME} {EVENT.UPDATE.ACTION} problem at {EVENT.UPDATE.DATE} {EVENT.UPDATE.TIME}.
	{EVENT.UPDATE.MESSAGE}

	Current problem status is {EVENT.STATUS}, age is {EVENT.AGE}, acknowledged: {EVENT.ACK.STATUS}.
	```
- Нажмите кнопку Добавить (Add) чтобы сохранить медиа-тип.


### Шаг 2: Назначаем медиа-тип пользователю
- Перейдите в Администрирование (Administration) или Пользователи → Пользователи (Users).
- Выберите пользователя, который будет получать оповещения, и нажмите на его имя.
- Перейдите на вкладку Медиа (Media) → нажмите Добавить (Add).
- Тип (Type): выберите "Notify-Bot" (ваш вебхук).
- Отправить на (Send to): укажите любой текст (это поле обязательно), например Bot.
- Нажмите Добавить (Add), а затем Обновить (Update).


### Шаг 3: Создаём "Действие" (Action) для отправки

Для администраторов "Ддействие" уже может быть создано. Или можно создать новое:

- Перейдите в "Оповещения" (Alerts) → «Действия» (Actions). Выберите тип действия триггеров (Trigger actions) и нажмите "Создать действие" (Create action).
- Имя (Name): задайте понятное имя, например "Отправка в Notify‑bot".
- (Необязательно) В пункте "Условия" (Conditions) нажмите "Добавить" (Add) и выберите нужные условия срабатывания (например, "Важность триггера" (Trigger severity) ≥ "Предупреждение" (Warning)).
- Перейдите на вкладку "Операции" (Operations) → в разделе «Операции» нажмите «Добавить» (Add).
- Тип операции (Operation type): оставьте "Отправить сообщение" (Send message).
- "Отправить пользователям/группам" (Send to Users/Groups): нажмите "Добавить" (Add) → "Пользователь" (User) → выберите пользователя, которому вы назначали медиатип.
- Send to media type: выберите наш созданный тип "Notify‑Bot".
- Нажмите "Добавить" (Add), чтобы сохранить операцию.
- В разделе "Операции восстановления" нажмите "Добавить" (Add).
- Операции: выберите "Оповещение всех вовлечённых" (Notify all involved). Сохраните, нажав "Добавить".
- В разделе "Операции обновления" нажмите "Добавить" (Add).
- Операции: выберите "Оповещение всех вовлечённых" (Notify all involved). Сохраните, нажав "Добавить".
- Нажмите "Добавить" внизу, чтобы сохранить действие.


---

## Лицензия
MIT
