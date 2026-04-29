package main

import (
    "fmt"
    "net"
    "strings"
)

// maskToken маскирует токен для логирования, показывая только первые 4 и последние 4 символа.
// Это нужно, чтобы случайно не вывести секретные данные в лог, но при этом сохранить
// возможность идентифицировать проблемный токен (например, при ошибке 401).
func maskToken(token string) string {
    if len(token) <= 8 {
        return "***"
    }
    return token[:4] + "..." + token[len(token)-4:]
}

// parseCIDR преобразует строку CIDR (например, "192.168.1.0/24" или "10.0.0.1")
// в объект *net.IPNet. Если маска не указана, для IPv4 добавляется "/32",
// для IPv6 – "/128", что означает один конкретный IP.
// Возвращает ошибку, если строка не является валидным IP или CIDR.
func parseCIDR(cidr string) (*net.IPNet, error) {
    if !strings.Contains(cidr, "/") {
        ip := net.ParseIP(cidr)
        if ip == nil {
            return nil, fmt.Errorf("invalid IP address: %s", cidr)
        }
        if ip.To4() != nil {
            cidr = cidr + "/32"
        } else {
            cidr = cidr + "/128"
        }
    }
    _, ipnet, err := net.ParseCIDR(cidr)
    if err != nil {
        return nil, fmt.Errorf("invalid CIDR: %s", cidr)
    }
    return ipnet, nil
}

// Global semaphore для ограничения количества одновременно работающих горутин отправки.
// В Python-версии использовался ThreadPoolExecutor(max_workers=20). Здесь мы используем
// буферизированный канал ёмкостью 100. Перед запуском горутины мы захватываем слот,
// после завершения – освобождаем. Это предотвращает создание неограниченного числа горутин
// при высокой нагрузке (например, 10 000 запросов/сек) и защищает от перерасхода памяти.
var semaphore = make(chan struct{}, 100)