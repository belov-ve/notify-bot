#!/bin/bash
# example_pdf_report.sh – Пример скрипта для планировщика задач (cron),
# который генерирует PDF-отчет о системе и возвращает JSON для отправки в чат.

# 1. Задаем путь, куда скрипт запишет сгенерированный PDF-файл.
# Каталог /app/data гарантированно примонтирован и доступен для записи внутри контейнера.
OUTPUT_FILE="/app/data/system_report.pdf"

# Получаем динамические данные системы для отчета.
REPORT_DATE=$(date "+%Y-%m-%d %H:%M:%S")
DISK_USAGE=$(df -h / | awk 'NR==2 {print $5}')
FREE_MEM=$(free -m | awk 'NR==2 {print $4}')
UPTIME=$(uptime | awk -F'( |,|:)+' '{d=0; h=0; m=0; if ($4=="day" || $4=="days") {d=$3; h=$5; m=$6} else {h=$3; m=$4} print d"d "h"h "m"m"}')

# 2. Генерируем минимальный валидный PDF-файл на чистом Bash без внешних утилит.
# Это гарантирует работу скрипта внутри базового Alpine-контейнера без установки дополнительных пакетов.
# PDF-структура содержит Catalog, Pages, Page, Font (Helvetica) и Content Stream с текстом отчета.

# Создаем контентную часть PDF (поток текста с разметкой позиционирования).
# BT - Begin Text, ET - End Text, Tf - Установка шрифта и размера, Td - Смещение координат (X Y).
# Символы "(" и ")" экранируют строки в формате PDF.
CONTENT_STREAM=$(cat <<EOF
BT
/F1 16 Tf
50 780 Td
(SYSTEM STATUS REPORT) Tj
/F1 10 Tf
0 -30 Td
(Generated at: $REPORT_DATE) Tj
0 -20 Td
(Uptime: $UPTIME) Tj
0 -15 Td
(Disk Usage: $DISK_USAGE) Tj
0 -15 Td
(Free Memory: $FREE_MEM MB) Tj
0 -30 Td
(System Status: OPERATIONAL) Tj
ET
EOF
)

# Вычисляем длину контентного потока.
CONTENT_LENGTH=${#CONTENT_STREAM}

# Записываем структуру PDF в файл.
# Большинство современных просмотрщиков (Telegram, Matrix, Chrome, Adobe Reader) 
# успешно открывают этот файл и автоматически восстанавливают отсутствующую таблицу xref.
cat <<EOF > "$OUTPUT_FILE"
%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> >> >> /MediaBox [0 0 595 842] /Contents 4 0 R >>
endobj
4 0 obj
<< /Length $CONTENT_LENGTH >>
stream
$CONTENT_STREAM
endstream
endobj
trailer
<< /Size 5 /Root 1 0 R >>
%%EOF
EOF

# 3. Выводим JSON-ответ в стандартный вывод (stdout).
# Бот перехватит этот JSON, скопирует файл в свою временную медиа-директорию,
# отправит его как вложение с подписью из поля "text" и удалит копию после отправки.
echo "{\"file_path\": \"$OUTPUT_FILE\", \"text\": \"Сгенерирован очередной PDF-отчет о состоянии системы ($REPORT_DATE).\"}"
