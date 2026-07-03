#!/bin/bash
# Скрипт для получения прогноза погоды через wttr.in с форматированием HTML для notify-bot.
# Скрипт запрашивает подробные данные о погоде в формате JSON и формирует красивую сводку.

# Устанавливаем тайм-аут для curl и количество попыток для стабильности
JSON_DATA=$(curl -s --max-time 10 --retry 3 "wttr.in/Moscow?format=j1&lang=ru")
CURL_EXIT=$?

# Проверяем успешность получения данных
if [ $CURL_EXIT -ne 0 ] || [ -z "$JSON_DATA" ]; then
    echo "❌ Ошибка: Не удалось получить данные о погоде от wttr.in (Exit: $CURL_EXIT)"
    exit 1
fi

# Извлекаем текущие условия
CURR_TEMP=$(echo "$JSON_DATA" | jq -r '.current_condition[0].temp_C')
CURR_DESC=$(echo "$JSON_DATA" | jq -r '.current_condition[0].lang_ru[0].value // .current_condition[0].weatherDesc[0].value')
WIND_DIR=$(echo "$JSON_DATA" | jq -r '.current_condition[0].winddir16Point')
WIND_SPEED=$(echo "$JSON_DATA" | jq -r '.current_condition[0].windspeedKmph')
CURR_HUMI=$(echo "$JSON_DATA" | jq -r '.current_condition[0].humidity')
CURR_RAIN=$(echo "$JSON_DATA" | jq -r '.current_condition[0].precipMM')

# Маппинг направления ветра на стрелку
case "$WIND_DIR" in
    N)   WIND_ARROW="↓" ;;
    NNE|NE|ENE) WIND_ARROW="↙" ;;
    E)   WIND_ARROW="←" ;;
    ESE|SE|SSE) WIND_ARROW="↖" ;;
    S)   WIND_ARROW="↑" ;;
    SSW|SW|WSW) WIND_ARROW="↗" ;;
    W)   WIND_ARROW="→" ;;
    WNW|NW|NNW) WIND_ARROW="↘" ;;
    *)   WIND_ARROW="" ;;
esac

# Извлекаем прогноз на сегодня
MIN_TEMP=$(echo "$JSON_DATA" | jq -r '.weather[0].mintempC')
MAX_TEMP=$(echo "$JSON_DATA" | jq -r '.weather[0].maxtempC')
DAY_DESC=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[4].lang_ru[0].value // .weather[0].hourly[4].weatherDesc[0].value')
DAY_RAIN=$(echo "$JSON_DATA" | jq -r '([.weather[0].hourly[].precipMM | tonumber] | add | (.*10 | round) / 10) // 0')
DAY_RAIN_CHANCE=$(echo "$JSON_DATA" | jq -r '([.weather[0].hourly[].chanceofrain | tonumber] | max) // 0')

SUNRISE_RAW=$(echo "$JSON_DATA" | jq -r '.weather[0].astronomy[0].sunrise')
SUNSET_RAW=$(echo "$JSON_DATA" | jq -r '.weather[0].astronomy[0].sunset')

# Функция для конвертации AM/PM времени в 24-часовой формат
convert_time() {
    local time_str="$1"
    local hh=$(echo "$time_str" | cut -d':' -f1)
    local mm=$(echo "$time_str" | cut -d':' -f2 | cut -d' ' -f1)
    local ampm=$(echo "$time_str" | cut -d' ' -f2)

    hh=$((10#$hh))
    if [ "$ampm" = "PM" ] && [ $hh -ne 12 ]; then
        hh=$((hh + 12))
    elif [ "$ampm" = "AM" ] && [ $hh -eq 12 ]; then
        hh=0
    fi
    printf "%02d:%02d" $hh $mm
}

SUNRISE=$(convert_time "$SUNRISE_RAW")
SUNSET=$(convert_time "$SUNSET_RAW")

# Добавляем знаки + к температуре, если она положительная
format_temp() {
    local temp="$1"
    if [ "${temp:0:1}" != "-" ] && [ "$temp" != "0" ]; then
        echo "+$temp"
    else
        echo "$temp"
    fi
}

CURR_TEMP_FMT=$(format_temp "$CURR_TEMP")
MIN_TEMP_FMT=$(format_temp "$MIN_TEMP")
MAX_TEMP_FMT=$(format_temp "$MAX_TEMP")

# Функция для экранирования спецсимволов HTML в динамическом тексте
escape_html() {
    local text="$1"
    # Сначала экранируем амперсанд, чтобы не дублировать экранирование в &lt; и &gt;
    text="${text//&/&amp;}"
    text="${text//</&lt;}"
    text="${text//>/&gt;}"
    echo "$text"
}

CURR_DESC_ESC=$(escape_html "$CURR_DESC")
DAY_DESC_ESC=$(escape_html "$DAY_DESC")

# Формируем итоговый HTML-вывод для отправки ботом.
# Префиксы <html> и </html> сообщают боту о необходимости включить парсинг HTML.
echo "<html><b>Погода в Москве на сегодня</b>

<b>Сейчас:</b> ${CURR_TEMP_FMT}°C, <i>$CURR_DESC_ESC</i>
Влажность: $CURR_HUMI%
Ветер: $WIND_ARROW $WIND_SPEED км/ч
Осадки: ${CURR_RAIN}мм

<b>Прогноз на день:</b>
Диапазон: <b>${MIN_TEMP_FMT}°C ... ${MAX_TEMP_FMT}°C</b>
Днем: <i>$DAY_DESC_ESC</i>
Осадки: ${DAY_RAIN}мм (вероятность ${DAY_RAIN_CHANCE}%)
Восход: <code>$SUNRISE</code> | Закат: <code>$SUNSET</code></html>"
