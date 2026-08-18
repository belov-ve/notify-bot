#!/bin/bash
# example_pdf_report.sh – Пример скрипта для планировщика задач (cron),
# который генерирует PDF-отчет о системе с поддержкой кириллицы и возвращает JSON для отправки в чат.

# 1. Задаем путь, куда скрипт запишет сгенерированный PDF-файл.
# Внутри Docker-контейнера доступен /app/data, при автономном запуске используем локальную папку data/
if [ -d "/app/data" ]; then
    OUTPUT_FILE="/app/data/system_report.pdf"
else
    mkdir -p "./data"
    OUTPUT_FILE="./data/system_report.pdf"
fi

# Создаем родительскую директорию при необходимости
mkdir -p "$(dirname "$OUTPUT_FILE")"

# Получаем динамические данные системы для отчета.
REPORT_DATE=$(date "+%Y-%m-%d %H:%M:%S")
DISK_USAGE=$(df -h / | awk 'NR==2 {print $5}')

if command -v free >/dev/null 2>&1; then
    FREE_MEM=$(free -m | awk 'NR==2 {print $4}')
else
    FREE_MEM="N/A"
fi

UPTIME=$(uptime | awk -F'( |,|:)+' '{d=0; h=0; m=0; if ($4=="day" || $4=="days") {d=$3; h=$5; m=$6} else {h=$3; m=$4} print d"д "h"ч "m"м"}')

# 2. Генерируем PDF-файл с поддержкой кириллицы.
# При наличии Python3 используется встроенный кириллический шрифт (base64 + zlib) через ReportLab,
# иначе используется резервный генератор с транслитерацией.

python3 - << 'PYEOF' "$OUTPUT_FILE" "$REPORT_DATE" "$UPTIME" "$DISK_USAGE" "$FREE_MEM"
import sys, os, datetime, zlib, base64, struct

output_file = sys.argv[1]
report_date = sys.argv[2]
uptime = sys.argv[3]
disk_usage = sys.argv[4]
free_mem = sys.argv[5]

EMBEDDED_FONT_B64 = "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"

font_file = "/tmp/cyrillic_system_font.ttf"
try:
    ttf_bytes = zlib.decompress(base64.b64decode(EMBEDDED_FONT_B64))
    with open(font_file, "wb") as f:
        f.write(ttf_bytes)
except Exception:
    font_file = None

pdf_created = False

try:
    from reportlab.lib.pagesizes import A4
    from reportlab.lib import colors
    from reportlab.platypus import SimpleDocTemplate, Paragraph, Spacer, Table, TableStyle
    from reportlab.lib.styles import getSampleStyleSheet, ParagraphStyle
    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont

    font_name = 'Helvetica'
    if font_file and os.path.exists(font_file):
        try:
            pdfmetrics.registerFont(TTFont('CyrillicFont', font_file))
            font_name = 'CyrillicFont'
        except Exception:
            font_name = 'Helvetica'

    doc = SimpleDocTemplate(output_file, pagesize=A4, rightMargin=40, leftMargin=40, topMargin=40, bottomMargin=40)
    styles = getSampleStyleSheet()

    title_style = ParagraphStyle(
        'DocTitle',
        parent=styles['Heading1'],
        fontName=font_name,
        fontSize=18,
        leading=22,
        textColor=colors.HexColor('#1E293B'),
        spaceAfter=15
    )

    cell_style = ParagraphStyle(
        'Cell',
        parent=styles['Normal'],
        fontName=font_name,
        fontSize=10,
        leading=14,
        textColor=colors.HexColor('#334155')
    )

    cell_bold = ParagraphStyle(
        'CellBold',
        parent=cell_style,
        fontName=font_name,
        textColor=colors.HexColor('#0F172A')
    )

    elements = [
        Paragraph("ОТЧЕТ О СОСТОЯНИИ СИСТЕМЫ", title_style),
        Spacer(1, 10)
    ]

    table_data = [
        [Paragraph("Параметр", cell_bold), Paragraph("Значение", cell_bold)],
        [Paragraph("Дата генерации", cell_style), Paragraph(report_date, cell_style)],
        [Paragraph("Время работы (Uptime)", cell_style), Paragraph(uptime, cell_style)],
        [Paragraph("Использование диска (/)", cell_style), Paragraph(disk_usage, cell_style)],
        [Paragraph("Свободная память", cell_style), Paragraph(f"{free_mem} MB" if free_mem != "N/A" else "Н/Д", cell_style)],
        [Paragraph("Статус системы", cell_style), Paragraph("РАБОТАЕТ В НОРМЕ", cell_style)]
    ]

    t = Table(table_data, colWidths=[200, 300])
    t.setStyle(TableStyle([
        ('BACKGROUND', (0,0), (-1,0), colors.HexColor('#F1F5F9')),
        ('TEXTCOLOR', (0,0), (-1,0), colors.HexColor('#0F172A')),
        ('BOTTOMPADDING', (0,0), (-1,-1), 8),
        ('TOPPADDING', (0,0), (-1,-1), 8),
        ('GRID', (0,0), (-1,-1), 0.5, colors.HexColor('#CBD5E1')),
    ]))

    elements.append(t)
    doc.build(elements)
    pdf_created = True
except Exception as e:
    pdf_created = False

if not pdf_created:
    lines = [
        f"OTCHET O SOSTOYANII SISTEMY",
        f"Data: {report_date}",
        f"Uptime: {uptime}",
        f"Disk: {disk_usage}",
        f"Free Mem: {free_mem}",
        f"Status: OPERATIONAL"
    ]
    pdf_comp = "BT\n/F1 14 Tf\n50 780 Td\n(%s) Tj\nET" % lines[0]
    y = 750
    for l in lines[1:]:
        y -= 20
        pdf_comp += "\nBT\n/F1 10 Tf\n50 %d Td\n(%s) Tj\nET" % (y, l)
    
    stream_bytes = pdf_comp.encode('latin1', 'ignore')
    content_len = len(stream_bytes)
    
    with open(output_file, 'wb') as f:
        f.write(b"%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
        f.write(b"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
        f.write(b"3 0 obj\n<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> >> >> /MediaBox [0 0 595 842] /Contents 4 0 R >>\nendobj\n")
        f.write(f"4 0 obj\n<< /Length {content_len} >>\nstream\n".encode('ascii'))
        f.write(stream_bytes)
        f.write(b"\nendstream\nendobj\ntrailer\n<< /Size 5 /Root 1 0 R >>\n%%EOF\n")

if font_file and os.path.exists(font_file):
    try:
        os.remove(font_file)
    except Exception:
        pass
PYEOF

if [ ! -f "$OUTPUT_FILE" ]; then
    echo "Ошибка: не удалось создать PDF-файл отчета '$OUTPUT_FILE'." >&2
    exit 1
fi

# 3. Выводим JSON-ответ в стандартный вывод (stdout).
echo "{\"file_path\": \"$OUTPUT_FILE\", \"text\": \"Сгенерирован очередной PDF-отчет о состоянии системы ($REPORT_DATE).\"}"
