#!/bin/bash
# example_pdf_report.sh – Пример скрипта для планировщика задач (cron),
# который генерирует PDF-отчет о системе с поддержкой кириллицы и возвращает JSON для отправки в чат.

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
OUTPUT_FILE="${TARGET_DIR}/system_report.pdf"

REPORT_DATE=$(date "+%Y-%m-%d %H:%M:%S")
DISK_USAGE=$(df -h / | awk 'NR==2 {print $5}')

if command -v free >/dev/null 2>&1; then
    FREE_MEM=$(free -m | awk 'NR==2 {print $4}')
else
    FREE_MEM="N/A"
fi

UPTIME=$(uptime | awk -F'( |,|:)+' '{d=0; h=0; m=0; if ($4=="day" || $4=="days") {d=$3; h=$5; m=$6} else {h=$3; m=$4} print d"д "h"ч "m"м"}')

# Генерация PDF через Python ReportLab с поддержкой кириллицы
python3 - << 'PYEOF' "$OUTPUT_FILE" "$REPORT_DATE" "$UPTIME" "$DISK_USAGE" "$FREE_MEM"
import sys, os, datetime

output_file = sys.argv[1]
report_date = sys.argv[2]
uptime = sys.argv[3]
disk_usage = sys.argv[4]
free_mem = sys.argv[5]

candidate_fonts = [
    "/System/Library/Fonts/Supplemental/Arial.ttf",
    "/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
    "/Library/Fonts/Arial Unicode.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf"
]

valid_font_path = None
for fp in candidate_fonts:
    if os.path.exists(fp):
        valid_font_path = fp
        break

try:
    from reportlab.lib.pagesizes import A4
    from reportlab.lib import colors
    from reportlab.platypus import SimpleDocTemplate, Paragraph, Spacer, Table, TableStyle
    from reportlab.lib.styles import getSampleStyleSheet, ParagraphStyle
    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont

    if valid_font_path:
        pdfmetrics.registerFont(TTFont('CyrillicFont', valid_font_path))
        font_name = 'CyrillicFont'
    else:
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
    sys.exit(0)
except Exception as e:
    sys.stderr.write(f"ReportLab error: {e}\n")
    sys.exit(1)
PYEOF

if [ ! -f "$OUTPUT_FILE" ]; then
    echo "Ошибка: не удалось создать PDF-файл отчета '$OUTPUT_FILE'." >&2
    exit 1
fi

echo "{\"file_path\": \"$OUTPUT_FILE\", \"text\": \"Сгенерирован очередной PDF-отчет о состоянии системы ($REPORT_DATE).\"}"
