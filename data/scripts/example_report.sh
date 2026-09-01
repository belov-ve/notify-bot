#!/bin/bash
# example_report.sh – Пример скрипта для планировщика задач (cron) или меню,
# который генерирует файл отчета на диске и возвращает JSON для отправки в чат.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATA_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

if [ -n "$OUTPUT_DIR" ]; then
    TARGET_DIR="$OUTPUT_DIR"
elif [ -d "/app/data" ]; then
    TARGET_DIR="/app/data"
else
    TARGET_DIR="$DATA_DIR"
fi

mkdir -p "$TARGET_DIR"
OUTPUT_FILE="${TARGET_DIR}/example_report.txt"

# 2. Генерируем текстовый файл отчета с динамическими данными
echo "=== ЕЖЕДНЕВНЫЙ ОТЧЕТ СИСТЕМЫ ===" > "$OUTPUT_FILE"
echo "Дата генерации: $(date)" >> "$OUTPUT_FILE"
echo "Свободное место на диске:" >> "$OUTPUT_FILE"
df -h / >> "$OUTPUT_FILE"
echo "===============================" >> "$OUTPUT_FILE"

# 3. Возвращаем JSON-ответ в stdout для перехвата ботом.
# Поля JSON:
#   - file_path: абсолютный путь к созданному файлу (внутри контейнера).
#   - text: сопроводительный текст (подпись к файлу в чате).
#
# Бот автоматически:
#   1. Прочитает JSON из stdout скрипта.
#   2. Обнаружит поле file_path и проверит наличие файла.
#   3. Скопирует файл в свою медиа-папку под уникальным именем для монопольного владения.
#   4. Доставит файл как вложение (document) в Telegram и Matrix.
#   5. Автоматически удалит свою временную копию после отправки.
echo "{\"file_path\": \"$OUTPUT_FILE\", \"text\": \"Сгенерирован очередной отчет о состоянии диска.\"}"
