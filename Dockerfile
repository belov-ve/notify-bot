# Мультистейдж-сборка для минимизации размера образа
FROM python:3.12-alpine3.20 AS builder
WORKDIR /app
COPY app/requirements.txt .
# Установка зависимостей Python в отдельный слой
RUN pip install --no-cache-dir -r requirements.txt

# Финальный образ
FROM python:3.12-alpine3.20
WORKDIR /app
# Копирование установленных пакетов из builder
COPY --from=builder /usr/local/lib/python3.12/site-packages /usr/local/lib/python3.12/site-packages
COPY --from=builder /usr/local/bin /usr/local/bin
# Копирование исходного кода
COPY app/app.py .
COPY app/matrix_sender.py .

ENV PYTHONUNBUFFERED=1
ENV LOG_LEVEL=INFO
# Диапазон портов для документации (реальные порты задаются в config.yml)
EXPOSE 8040-8050

CMD ["python", "app.py"]