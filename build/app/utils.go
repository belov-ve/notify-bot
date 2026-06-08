package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// JSONPair структура для хранения пары ключ-значение с сохранением порядка.
type JSONPair struct {
	Key   string
	Value interface{}
}

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

// DecodeOrderedJSON декодирует JSON вручную, чтобы сохранить порядок ключей.
// Стандартный json.Unmarshal в map[] этого не гарантирует.
func DecodeOrderedJSON(r io.Reader) ([]JSONPair, error) {
	dec := json.NewDecoder(r)
	
	// Ожидаем начало объекта '{'
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := t.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected JSON object start '{'")
	}

	var pairs []JSONPair
	for dec.More() {
		// Читаем ключ
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", t)
		}

		// Читаем значение
		var val interface{}
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}

		pairs = append(pairs, JSONPair{Key: key, Value: val})
	}

	// Ожидаем конец объекта '}'
	t, err = dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := t.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("expected JSON object end '}'")
	}

	return pairs, nil
}

// createPlaceholderImage динамически генерирует серую картинку 300x100
// с красной рамкой и красным крестом в качестве заглушки при ошибках.
func createPlaceholderImage(path string) error {
	img := image.NewRGBA(image.Rect(0, 0, 300, 100))
	
	// Заполняем фон светло-серым цветом
	gray := color.RGBA{220, 220, 220, 255}
	for x := 0; x < 300; x++ {
		for y := 0; y < 100; y++ {
			img.Set(x, y, gray)
		}
	}
	
	// Рисуем красную рамку по периметру
	red := color.RGBA{255, 0, 0, 255}
	for x := 0; x < 300; x++ {
		img.Set(x, 0, red)
		img.Set(x, 1, red)
		img.Set(x, 98, red)
		img.Set(x, 99, red)
	}
	for y := 0; y < 100; y++ {
		img.Set(0, y, red)
		img.Set(1, y, red)
		img.Set(298, y, red)
		img.Set(299, y, red)
	}

	// Рисуем красный крест (букву X) в центре картинки
	for i := 10; i < 90; i++ {
		img.Set(i+100, i, red)
		img.Set(i+100, i+1, red)
		img.Set(200-i, i, red)
		img.Set(200-i, i+1, red)
	}

	// Создаем и записываем PNG файл на диск
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// getMediaDir возвращает путь к директории хранения медиафайлов.
// Путь строится относительно директории базы данных (из DB_PATH) или используется значение по умолчанию /app/data/media.
func getMediaDir() string {
	mediaDir := "/app/data/media"
	if envPath := os.Getenv("DB_PATH"); envPath != "" {
		mediaDir = filepath.Join(filepath.Dir(envPath), "media")
	}
	return mediaDir
}