#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Многопортовый уведомитель для Technitium DNS Server (и любых JSON webhook).
Принимает POST на /notify, формирует сообщение, асинхронно отправляет в Telegram и/или Matrix.
Поддерживает несколько экземпляров (разные порты, свои настройки).
"""

import sys
import os

try:
    import yaml
    import logging
    import ipaddress
    import time
    import traceback
    from multiprocessing import Process
    from concurrent.futures import ThreadPoolExecutor
    from flask import Flask, request, jsonify, abort
    from waitress import serve
    from matrix_sender import MatrixSender
except Exception as e:
    print(f"FATAL: Import error: {e}", file=sys.stderr)
    sys.exit(1)

# ============================================================================
# Конфигурация логирования
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

# Пул потоков для асинхронных отправок (Telegram и Matrix)
executor = ThreadPoolExecutor(max_workers=20)


# ============================================================================
# Вспомогательные функции для Telegram
# ============================================================================
def mask_token(token, show_full=False):
    """Маскирует токен в логах, кроме режима DEBUG."""
    if show_full and LOG_LEVEL == "DEBUG":
        return token
    if not token or len(token) <= 8:
        return "***"
    return token[:4] + "..." + token[-4:]


def send_telegram_message_async(text, bot_token, chat_id, instance_name, retry_count=3, retry_delay=2):
    """Асинхронно отправляет сообщение в Telegram (в фоновом потоке)."""
    executor.submit(
        _send_telegram_message_sync,
        text, bot_token, chat_id, instance_name, retry_count, retry_delay
    )


def _send_telegram_message_sync(text, bot_token, chat_id, instance_name, retry_count, retry_delay):
    """Синхронная отправка в Telegram с повторными попытками (экспоненциальная задержка)."""
    import requests
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
                logger.info(f"Telegram: message sent to chat {chat_id} (instance '{instance_name}')")
                return True
            else:
                if resp.status_code == 401:
                    logger.error(
                        f"Telegram API error 401 for instance '{instance_name}'. "
                        f"Bot token: {masked_token}, chat_id: {chat_id}"
                    )
                else:
                    logger.warning(f"Telegram API error {resp.status_code} for instance '{instance_name}': {resp.text}")
        except Exception as e:
            logger.warning(f"Telegram attempt {attempt} failed: {e}")
        if attempt < retry_count:
            time.sleep(delay)
            delay *= 2
    logger.error(f"Telegram: all attempts failed for instance '{instance_name}'")
    return False


# ============================================================================
# Загрузка конфигурации
# ============================================================================
def load_config():
    """Загружает и проверяет структуру config.yml."""
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
# Создание Flask-приложения для одного экземпляра
# ============================================================================
def create_app(instance):
    """Фабрика приложений: каждый экземпляр получает свой порт, настройки Telegram/Matrix."""
    app = Flask(__name__)

    app.config['NAME'] = instance['name']
    app.config['PORT'] = instance['port']

    # Если экземпляр отключён, возвращаем заглушку (503 на все запросы)
    if not instance.get('enabled', True):
        logger.info(f"Instance '{app.config['NAME']}' is disabled globally")
        @app.route('/health', methods=['GET'])
        def health_disabled():
            return jsonify({"status": "disabled"}), 503
        @app.route('/notify', methods=['POST'])
        def notify_disabled():
            return jsonify({"status": "disabled", "message": "Instance is disabled"}), 503
        return app

    # Разбор разрешённых IP-сетей (CIDR)
    allowed_nets = []
    for cidr in instance.get('allowed_ips', []):
        try:
            net = ipaddress.ip_network(cidr)
            allowed_nets.append(net)
            logger.debug(f"Instance '{app.config['NAME']}': allowed network {net}")
        except ValueError as e:
            logger.error(f"Invalid CIDR '{cidr}' for instance '{app.config['NAME']}': {e}")
    app.config['ALLOWED_IPS'] = allowed_nets

    # ----- Telegram -----
    tg_cfg = instance.get('telegram')
    if tg_cfg and isinstance(tg_cfg, dict):
        tg_enabled = tg_cfg.get('enabled', True)
        if tg_enabled:
            bot_token = tg_cfg.get('bot_token')
            chat_id = tg_cfg.get('chat_id')
            if bot_token and chat_id:
                app.config['TELEGRAM_ENABLED'] = True
                app.config['BOT_TOKEN'] = bot_token
                app.config['CHAT_ID'] = chat_id
                app.config['TG_RETRY_COUNT'] = tg_cfg.get('retry_count', 3)
                app.config['TG_RETRY_DELAY'] = tg_cfg.get('retry_delay', 2)
                logger.debug(f"Instance '{app.config['NAME']}': Telegram enabled")
            else:
                logger.warning(f"Instance '{app.config['NAME']}': Telegram missing bot_token/chat_id, disabled")
                app.config['TELEGRAM_ENABLED'] = False
        else:
            app.config['TELEGRAM_ENABLED'] = False
    else:
        app.config['TELEGRAM_ENABLED'] = False

    # ----- Matrix (без шифрования) -----
    matrix_cfg = instance.get('matrix')
    if matrix_cfg and isinstance(matrix_cfg, dict):
        mx_enabled = matrix_cfg.get('enabled', True)
        if mx_enabled:
            homeserver = matrix_cfg.get('homeserver')
            username = matrix_cfg.get('username')
            password = matrix_cfg.get('password')
            access_token = matrix_cfg.get('access_token')
            room_id = matrix_cfg.get('room_id')
            retry_cnt = matrix_cfg.get('retry_count', 3)
            retry_delay = matrix_cfg.get('retry_delay', 2)

            if homeserver and username and room_id and (access_token or password):
                app.config['MATRIX_ENABLED'] = True
                app.config['MATRIX_SENDER'] = MatrixSender(
                    homeserver=homeserver,
                    username=username,
                    room_id=room_id,
                    password=password,
                    access_token=access_token,
                    retry_count=retry_cnt,
                    retry_delay=retry_delay
                )
                logger.debug(f"Instance '{app.config['NAME']}': Matrix enabled")
            else:
                logger.warning(f"Instance '{app.config['NAME']}': Matrix missing or invalid credentials, disabled")
                app.config['MATRIX_ENABLED'] = False
        else:
            app.config['MATRIX_ENABLED'] = False
    else:
        app.config['MATRIX_ENABLED'] = False

    if not (app.config.get('TELEGRAM_ENABLED') or app.config.get('MATRIX_ENABLED')):
        logger.warning(f"Instance '{app.config['NAME']}': no active notification channels")

    # ------------------------------------------------------------
    # Эндпоинты
    # ------------------------------------------------------------
    @app.route('/health', methods=['GET'])
    def health():
        logger.debug(f"Health check from {request.remote_addr} for '{app.config['NAME']}'")
        return jsonify({"status": "ok"})

    @app.route('/notify', methods=['POST'])
    def notify():
        req_id = str(time.time_ns())[-6:]   # короткий ID для трейсинга
        client_ip = request.remote_addr
        instance_name = app.config['NAME']

        logger.debug(f"[{req_id}] Request from {client_ip} to '{instance_name}'")

        # Проверка IP по разрешённым сетям
        try:
            ip_obj = ipaddress.ip_address(client_ip)
        except ValueError:
            logger.error(f"[{req_id}] Invalid IP: {client_ip}")
            abort(400)

        allowed = any(ip_obj in net for net in app.config['ALLOWED_IPS'])
        if not allowed:
            logger.warning(f"[{req_id}] Blocked {client_ip} for '{instance_name}'")
            abort(403)

        # Получение JSON
        data = request.get_json(silent=True)
        if data is None:
            logger.error(f"[{req_id}] No JSON body")
            abort(400)
        logger.debug(f"[{req_id}] JSON: {data}")

        # Формирование текста сообщения
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

        # Асинхронная отправка в Telegram
        if app.config.get('TELEGRAM_ENABLED'):
            send_telegram_message_async(
                text=message,
                bot_token=app.config['BOT_TOKEN'],
                chat_id=app.config['CHAT_ID'],
                instance_name=instance_name,
                retry_count=app.config['TG_RETRY_COUNT'],
                retry_delay=app.config['TG_RETRY_DELAY']
            )

        # Асинхронная отправка в Matrix
        if app.config.get('MATRIX_ENABLED'):
            matrix = app.config['MATRIX_SENDER']
            executor.submit(matrix.send_message, message)

        logger.info(f"[{req_id}] Accepted from {client_ip} for '{instance_name}'")
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
        logger.exception(f"Instance '{name}': Waitress error: {e}")
        sys.exit(1)


# ============================================================================
# Главный процесс
# ============================================================================
def main():
    logger.debug("Entering main()")
    logger.info("Starting notify-bot (Telegram + Matrix support)")

    instances = load_config()
    processes = []
    for inst in instances:
        # Пропускаем отключённые экземпляры (enabled: false)
        if not inst.get('enabled', True):
            logger.info(f"Skipping disabled instance '{inst['name']}' (port {inst['port']})")
            continue

        # Проверяем, что есть хотя бы один активный канал (Telegram или Matrix)
        has_telegram = bool(inst.get('telegram', {}).get('enabled', False) and
                            inst.get('telegram', {}).get('bot_token') and
                            inst.get('telegram', {}).get('chat_id'))
        has_matrix = bool(inst.get('matrix', {}).get('enabled', False) and
                          inst.get('matrix', {}).get('homeserver') and
                          inst.get('matrix', {}).get('username') and
                          inst.get('matrix', {}).get('room_id') and
                          (inst.get('matrix', {}).get('access_token') or inst.get('matrix', {}).get('password')))
        if not (has_telegram or has_matrix):
            logger.error(f"Skipping instance '{inst['name']}' (port {inst['port']}) – no active notification channels")
            continue

        p = Process(target=run_instance, args=(inst,))
        p.start()
        processes.append(p)
        logger.info(f"Spawned process for '{inst['name']}' (port {inst['port']}, PID {p.pid})")

    if not processes:
        logger.error("No valid instances to run. Exiting.")
        sys.exit(1)

    # Мониторинг дочерних процессов
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