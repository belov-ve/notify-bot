#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Отправка сообщений в Matrix (Element) через простой HTTP API.
Без шифрования. Поддерживает логин по паролю или готовому access_token.
"""

import logging
import time
import requests

logger = logging.getLogger(__name__)


class MatrixSender:
    def __init__(self, homeserver, username, room_id, password=None, access_token=None,
                 retry_count=3, retry_delay=2):
        """
        homeserver: URL Matrix-сервера (например, https://matrix.example.com)
        username: полное имя пользователя (@bot:example.com)
        room_id: идентификатор комнаты
        password или access_token: способ аутентификации
        retry_count, retry_delay: параметры повторных попыток
        """
        self.homeserver = homeserver.rstrip('/')
        self.username = username
        self.password = password
        self.access_token = access_token
        self.room_id = room_id
        self.retry_count = retry_count
        self.retry_delay = retry_delay
        self._session_token = None

    def _login(self):
        """Получение access_token через пароль (если не задан готовый токен)."""
        if self.access_token:
            self._session_token = self.access_token
            return True
        if not self.password:
            logger.error("Matrix requires password or access_token")
            return False

        url = f"{self.homeserver}/_matrix/client/v3/login"
        payload = {
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": self.username},
            "password": self.password
        }
        try:
            resp = requests.post(url, json=payload, timeout=10)
            if resp.status_code == 200:
                data = resp.json()
                self._session_token = data.get("access_token")
                logger.info(f"Matrix: logged in as {self.username}")
                return True
            else:
                logger.error(f"Matrix login failed: {resp.status_code} {resp.text}")
                return False
        except Exception as e:
            logger.error(f"Matrix login error: {e}")
            return False

    def _send_sync(self, text, txn_id):
        """Синхронная отправка одного сообщения (одна попытка)."""
        if not self._session_token:
            if not self._login():
                return False

        url = f"{self.homeserver}/_matrix/client/v3/rooms/{self.room_id}/send/m.room.message/{txn_id}"
        headers = {"Authorization": f"Bearer {self._session_token}"}
        body = {"msgtype": "m.text", "body": text}

        try:
            resp = requests.put(url, json=body, headers=headers, timeout=10)
            if resp.status_code == 200:
                logger.info(f"Matrix: message sent to room {self.room_id}")
                return True
            elif resp.status_code == 401:
                logger.warning("Matrix: token expired, will re-login")
                self._session_token = None
                return False
            else:
                logger.warning(f"Matrix error {resp.status_code}: {resp.text}")
                return False
        except Exception as e:
            logger.warning(f"Matrix exception: {e}")
            return False

    def send_message(self, text):
        """Отправка сообщения с повторными попытками (экспоненциальная задержка)."""
        delay = self.retry_delay
        for attempt in range(1, self.retry_count + 1):
            txn_id = f"{int(time.time() * 1000)}-{attempt}"
            success = self._send_sync(text, txn_id)
            if success:
                return True
            if attempt < self.retry_count:
                logger.debug(f"Matrix retry {attempt}/{self.retry_count} in {delay}s")
                time.sleep(delay)
                delay *= 2
        logger.error(f"Matrix: all {self.retry_count} attempts failed for room {self.room_id}")
        return False