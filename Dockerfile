FROM python:3.12-alpine3.20 AS builder
WORKDIR /app
COPY app/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

FROM python:3.12-alpine3.20
WORKDIR /app
COPY --from=builder /usr/local/lib/python3.12/site-packages /usr/local/lib/python3.12/site-packages
COPY --from=builder /usr/local/bin /usr/local/bin
COPY app/app.py .

ENV PYTHONUNBUFFERED=1
ENV LOG_LEVEL=INFO

# Документация портов (реальные порты задаются в config.yml)
EXPOSE 8040-8042

CMD ["python", "app.py"]
