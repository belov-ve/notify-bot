#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Многопортовый уведомитель для Technitium DNS Server.
Работает в режиме network_mode: host. Видит реальный IP клиента.
Каждый экземпляр – отдельный процесс с Waitress.
"""

import os
import sys
import yaml
import logging
import ipaddress
import requests
import time
import traceback
from multiprocessing import Process
from concurrent.futures import ThreadPoolExecutor
from flask import Flask, request, jsonify, abort
from waitress import serve

# ============================================================================
# Конфигурация
# ============================================================================
CONFIG_PATH = "/app/config.yml"
LOG_LEVEL = os.environ.get("LOG_LEVEL", "INFO").upper()

logging.basicConfig(
    stream=sys.stdout,
    level=getattr(logging, LOG_LEVEL, logging.INFO),
    format="%(asctime)s.%(msecs)03d - %(levelname)s - [%(filename)s:%(lineno)d] - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S"
)
logger = logging.getLogger(__name__)

executor = ThreadPoolExecutor(max_workers=10)


# ============================================================================
# Утилиты
# ============================================================================
def mask_token(token, show_full=False):
    """Безопасное отображение токена в логах."""
    if show_full and LOG_LEVEL == "DEBUG":
        return token
    if not token or len(token) <= 8:
        return "***"
    return token[:4] + "..." + token[-4:]


# ============================================================================
# Загрузка конфигурации
# ============================================================================
def load_config():
    logger.debug(f"Loading config from {CONFIG_PATH}")
    if not os.path.exists(CONFIG_PATH):
        logger.error(f"Config file not found: {CONFIG_PATH}")
        sys.exit(1)
    try:
        with open(CONFIG_PATH, 'r') as f:
            data = yaml.safe_load(f)
        instances = data.get('instances', [])
        if not instances:
            logger.error("No instances defined in config")
            sys.exit(1)
        for idx, inst in enumerate(instances):
            if 'name' not in inst:
                logger.error(f"Instance {idx} missing 'name' field")
                sys.exit(1)
        logger.debug(f"Loaded {len(instances)} instances")
        return instances
    except Exception as e:
        logger.exception(f"Config load error: {e}")
        sys.exit(1)


# ============================================================================
# Асинхронная отправка в Telegram
# ============================================================================
def send_telegram_message_async(text, bot_token, chat_id, instance_name, retry_count=3, retry_delay=2):
    executor.submit(
        _send_telegram_message_sync,
        text, bot_token, chat_id, instance_name, retry_count, retry_delay
    )


def _send_telegram_message_sync(text, bot_token, chat_id, instance_name, retry_count, retry_delay):
    url = f"https://api.telegram.org/bot{bot_token}/sendMessage"
    payload = {"chat_id": chat_id, "text": text, "parse_mode": "HTML"}
    delay = retry_delay
    show_full = (LOG_LEVEL == "DEBUG")
    masked_token = mask_token(bot_token, show_full)

    for attempt in range(1, retry_count + 1):
        logger.debug(f"Telegram attempt {attempt}/{retry_count} for instance '{instance_name}'")
        try:
            resp = requests.post(url, json=payload, timeout=10)
            if resp.status_code == 200:
                logger.info(f"Message sent to chat {chat_id} (instance '{instance_name}')")
                return True
            else:
                if resp.status_code == 401:
                    logger.error(
                        f"Telegram API error 401 (Unauthorized) for instance '{instance_name}'. "
                        f"Bot token: {masked_token}, chat_id: {chat_id}"
                    )
                else:
                    logger.warning(
                        f"Telegram API error {resp.status_code} for instance '{instance_name}': {resp.text}"
                    )
        except requests.exceptions.Timeout:
            logger.warning(f"Attempt {attempt}/{retry_count} timeout for instance '{instance_name}'")
        except requests.exceptions.ConnectionError as e:
            logger.warning(f"Attempt {attempt}/{retry_count} connection error for instance '{instance_name}': {e}")
        except Exception as e:
            logger.warning(f"Attempt {attempt}/{retry_count} unexpected error for instance '{instance_name}': {e}")
            logger.debug(traceback.format_exc())

        if attempt < retry_count:
            logger.debug(f"Waiting {delay}s before retry for instance '{instance_name}'")
            time.sleep(delay)
            delay *= 2
        else:
            logger.error(f"All {retry_count} attempts exhausted for instance '{instance_name}'")

    return False


# ============================================================================
# Flask-приложение для одного экземпляра
# ============================================================================
def create_app(instance):
    app = Flask(__name__)

    app.config['NAME'] = instance['name']
    app.config['BOT_TOKEN'] = instance['bot_token']
    app.config['CHAT_ID'] = instance['chat_id']
    app.config['RETRY_COUNT'] = instance.get('retry_count', 3)
    app.config['RETRY_DELAY'] = instance.get('retry_delay', 2)
    app.config['PORT'] = instance['port']

    allowed_nets = []
    for cidr in instance['allowed_ips']:
        try:
            net = ipaddress.ip_network(cidr)
            allowed_nets.append(net)
            logger.debug(f"Instance '{app.config['NAME']}' (port {app.config['PORT']}): allowed network {net}")
        except ValueError as e:
            logger.error(f"Invalid CIDR '{cidr}' for instance '{app.config['NAME']}': {e}")
            sys.exit(1)
    app.config['ALLOWED_IPS'] = allowed_nets

    @app.route('/health', methods=['GET'])
    def health():
        logger.debug(f"Health check from {request.remote_addr} for instance '{app.config['NAME']}'")
        return jsonify({"status": "ok"})

    @app.route('/notify', methods=['POST'])
    def notify():
        req_id = str(time.time_ns())[-6:]
        client_ip = request.remote_addr
        instance_name = app.config['NAME']

        if LOG_LEVEL == "DEBUG":
            forwarded = request.headers.get('X-Forwarded-For')
            logger.debug(f"[{req_id}] Headers: X-Forwarded-For={forwarded}, remote_addr={client_ip}")

        logger.debug(f"[{req_id}] Request from {client_ip} to instance '{instance_name}' (port {app.config['PORT']})")

        # Проверка IP
        try:
            ip_obj = ipaddress.ip_address(client_ip)
        except ValueError:
            logger.error(f"[{req_id}] Invalid IP format: {client_ip} for instance '{instance_name}'")
            abort(400)

        allowed = any(ip_obj in net for net in app.config['ALLOWED_IPS'])
        if not allowed:
            logger.warning(
                f"[{req_id}] Blocked {client_ip} for instance '{instance_name}' "
                f"(allowed: {app.config['ALLOWED_IPS']})"
            )
            abort(403)

        # Получение JSON
        data = request.get_json(silent=True)
        if data is None:
            logger.error(f"[{req_id}] No JSON body for instance '{instance_name}'")
            abort(400)
        logger.debug(f"[{req_id}] JSON: {data}")

        # Формирование сообщения
        lines = []
        if 'text' in data:
            lines.append(str(data['text']))
            for k, v in data.items():
                if k != 'text':
                    lines.append(f"{k}: {v}")
        else:
            for k, v in data.items():
                lines.append(f"{k}: {v}")
        message = "\n".join(lines)
        logger.debug(f"[{req_id}] Message:\n{message}")

        # Асинхронная отправка
        send_telegram_message_async(
            text=message,
            bot_token=app.config['BOT_TOKEN'],
            chat_id=app.config['CHAT_ID'],
            instance_name=instance_name,
            retry_count=app.config['RETRY_COUNT'],
            retry_delay=app.config['RETRY_DELAY']
        )

        logger.info(f"[{req_id}] Accepted from {client_ip} for instance '{instance_name}'")
        return jsonify({"status": "accepted"}), 202

    return app


# ============================================================================
# Запуск одного экземпляра в отдельном процессе
# ============================================================================
def run_instance(instance):
    name = instance['name']
    port = instance['port']
    app = create_app(instance)
    logger.info(f"Starting instance '{name}' on port {port}")
    try:
        serve(app, host='0.0.0.0', port=port, threads=20)
    except OSError as e:
        logger.error(f"Instance '{name}': cannot bind port {port}: {e}")
        sys.exit(1)
    except Exception as e:
        logger.exception(f"Instance '{name}': Waitress error on port {port}: {e}")
        sys.exit(1)


# ============================================================================
# Главный процесс
# ============================================================================
def main():
    logger.info("Starting notify-tbot")
    instances = load_config()
    processes = []
    for inst in instances:
        p = Process(target=run_instance, args=(inst,))
        p.start()
        processes.append(p)
        logger.info(f"Spawned process for instance '{inst['name']}' (port {inst['port']}, PID {p.pid})")

    try:
        while True:
            for p in processes:
                if not p.is_alive():
                    logger.error(f"Process {p.pid} died unexpectedly")
                    sys.exit(1)
            time.sleep(5)
    except KeyboardInterrupt:
        logger.info("Shutting down...")
        for p in processes:
            p.terminate()
            p.join()
        logger.info("Done")


if __name__ == '__main__':
    main()