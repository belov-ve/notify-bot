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

# Функция для перевода английских статусов wttr.in на русский язык
translate_desc() {
    local desc="$1"
    # Приводим к нижнему регистру и обрезаем пробелы по краям
    local clean_desc=$(echo "$desc" | tr '[:upper:]' '[:lower:]' | xargs)
    
    case "$clean_desc" in
        "patchy rain nearby"|"patchy rain possible"|"patchy rain in nearby") echo "Местами кратковременный дождь" ;;
        "patchy light rain") echo "Местами небольшой дождь" ;;
        "light rain") echo "Небольшой дождь" ;;
        "moderate rain at times"|"moderate rain") echo "Умеренный дождь" ;;
        "heavy rain at times"|"heavy rain") echo "Сильный дождь" ;;
        "patchy snow nearby"|"patchy snow possible"|"patchy snow in nearby") echo "Местами небольшой снег" ;;
        "patchy sleet nearby"|"patchy sleet possible"|"patchy sleet in nearby") echo "Местами небольшой мокрый снег" ;;
        "patchy freezing drizzle nearby"|"patchy freezing drizzle possible"|"patchy freezing drizzle in nearby") echo "Местами изморозь" ;;
        "thundery outbreaks nearby"|"thundery outbreaks possible"|"thundery outbreaks in nearby") echo "Местами грозы" ;;
        "blowing snow") echo "Метель" ;;
        "blizzard") echo "Снежная буря" ;;
        "mist") echo "Дымка" ;;
        "fog") echo "Туман" ;;
        "freezing fog") echo "Ледяной туман" ;;
        "patchy light drizzle") echo "Местами легкая морось" ;;
        "light drizzle") echo "Легкая морось" ;;
        "freezing drizzle") echo "Замерзающая морось" ;;
        "heavy freezing drizzle") echo "Сильная замерзающая морось" ;;
        "moderate or heavy rain shower"|"moderate or heavy rain showers") echo "Умеренный или сильный ливень" ;;
        "torrential rain shower"|"torrential rain showers") echo "Сильный ливневый дождь" ;;
        "light rain shower"|"light rain showers") echo "Небольшой ливневый дождь" ;;
        "light sleet showers"|"light sleet shower") echo "Небольшой мокрый снег" ;;
        "moderate or heavy sleet showers"|"moderate or heavy sleet shower") echo "Умеренный или сильный мокрый снег" ;;
        "light snow showers"|"light snow shower") echo "Небольшой снегопад" ;;
        "moderate or heavy snow showers"|"moderate or heavy snow shower") echo "Умеренный или сильный снегопад" ;;
        "light showers of ice pellets"|"light shower of ice pellets") echo "Небольшой ледяной дождь" ;;
        "moderate or heavy showers of ice pellets"|"moderate or heavy shower of ice pellets") echo "Умеренный или сильный ледяной дождь" ;;
        "patchy light rain with thunder") echo "Местами небольшой дождь с грозой" ;;
        "moderate or heavy rain with thunder") echo "Умеренный или сильный дождь с грозой" ;;
        "patchy light snow with thunder") echo "Местами небольшой снег с грозой" ;;
        "moderate or heavy snow with thunder") echo "Умеренный или сильный снег с грозой" ;;
        "partly cloudy") echo "Переменная облачность" ;;
        "cloudy") echo "Облачно" ;;
        "overcast") echo "Пасмурно" ;;
        "sunny") echo "Солнечно" ;;
        "clear") echo "Ясно" ;;
        *) echo "$desc" ;;
    esac
}

# Извлекаем текущие условия
CURR_TEMP=$(echo "$JSON_DATA" | jq -r '.current_condition[0].temp_C')
CURR_DESC_RAW=$(echo "$JSON_DATA" | jq -r '.current_condition[0].lang_ru[0].value // .current_condition[0].weatherDesc[0].value')
CURR_DESC=$(translate_desc "$CURR_DESC_RAW")
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
DAY_RAIN=$(echo "$JSON_DATA" | jq -r '([.weather[0].hourly[].precipMM | tonumber] | add | (.*10 | round) / 10) // 0')
DAY_RAIN_CHANCE=$(echo "$JSON_DATA" | jq -r '([.weather[0].hourly[].chanceofrain | tonumber] | max) // 0')

# Почасовой прогноз по частям суток
MORNING_TEMP=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[3].tempC')
MORNING_DESC_RAW=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[3].lang_ru[0].value // .weather[0].hourly[3].weatherDesc[0].value')
MORNING_DESC=$(translate_desc "$MORNING_DESC_RAW")

AFTERNOON_TEMP=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[5].tempC')
AFTERNOON_DESC_RAW=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[5].lang_ru[0].value // .weather[0].hourly[5].weatherDesc[0].value')
AFTERNOON_DESC=$(translate_desc "$AFTERNOON_DESC_RAW")

EVENING_TEMP=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[7].tempC')
EVENING_DESC_RAW=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[7].lang_ru[0].value // .weather[0].hourly[7].weatherDesc[0].value')
EVENING_DESC=$(translate_desc "$EVENING_DESC_RAW")

NIGHT_TEMP=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[1].tempC')
NIGHT_DESC_RAW=$(echo "$JSON_DATA" | jq -r '.weather[0].hourly[1].lang_ru[0].value // .weather[0].hourly[1].weatherDesc[0].value')
NIGHT_DESC=$(translate_desc "$NIGHT_DESC_RAW")

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

MORNING_TEMP_FMT=$(format_temp "$MORNING_TEMP")
AFTERNOON_TEMP_FMT=$(format_temp "$AFTERNOON_TEMP")
EVENING_TEMP_FMT=$(format_temp "$EVENING_TEMP")
NIGHT_TEMP_FMT=$(format_temp "$NIGHT_TEMP")

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
MORNING_DESC_ESC=$(escape_html "$MORNING_DESC")
AFTERNOON_DESC_ESC=$(escape_html "$AFTERNOON_DESC")
EVENING_DESC_ESC=$(escape_html "$EVENING_DESC")
NIGHT_DESC_ESC=$(escape_html "$NIGHT_DESC")

# Формируем итоговый HTML-вывод для отправки ботом.
# Префиксы <html> и </html> сообщают боту о необходимости включить парсинг HTML.
echo "<html><b>Погода в Москве на сегодня</b>

<b>Сейчас:</b> ${CURR_TEMP_FMT}°C, <i>$CURR_DESC_ESC</i>
Влажность: $CURR_HUMI%
Ветер: $WIND_ARROW $WIND_SPEED км/ч
Осадки: ${CURR_RAIN}мм

<b>Прогноз на день:</b>
Диапазон: <b>${MIN_TEMP_FMT}°C ... ${MAX_TEMP_FMT}°C</b>
Осадки: ${DAY_RAIN}мм (вероятность ${DAY_RAIN_CHANCE}%)

🌅 <b>Утро (09:00):</b> ${MORNING_TEMP_FMT}°C, <i>$MORNING_DESC_ESC</i>
☀️ <b>День (15:00):</b> ${AFTERNOON_TEMP_FMT}°C, <i>$AFTERNOON_DESC_ESC</i>
🌙 <b>Вечер (21:00):</b> ${EVENING_TEMP_FMT}°C, <i>$EVENING_DESC_ESC</i>
🌌 <b>Ночь (03:00):</b> ${NIGHT_TEMP_FMT}°C, <i>$NIGHT_DESC_ESC</i>

Восход: <code>$SUNRISE</code> | Закат: <code>$SUNSET</code></html>"
