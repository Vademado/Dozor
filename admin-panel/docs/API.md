> Новый раздел «Консоль камеры»: [код, HTTP/WebSocket API и инструкция](CAMERA_CONSOLE.md).

> Актуальный режим: штатная страница go2rtc встроена в iframe; собственный медиадвижок и прокси не запускаются. См. [GO2RTC_EMBED.md](GO2RTC_EMBED.md). Описания прежнего пути видео ниже сохранены как история реализации.

# HTTP API

Базовый адрес: `http://127.0.0.1:8080`. Входной JSON: `Content-Type: application/json`, один объект, не более 16 КиБ. Неизвестные поля отклоняются. Успех возвращает JSON, кроме ответов 204 и SSE/экспорта. Ошибка: `{"error":"текст","field":"необязательное поле"}`.

## Доступ

Операторские API используют HTTP Basic `admin` / `GUARD_ADMIN_PASSWORD`, если пароль задан. Локально по умолчанию пароль не требуется; Host должен быть localhost/loopback. Запросы с посторонним Origin/Sec-Fetch-Site блокируются. Это API локальной панели, не публичный CORS API.

Агент использует **отдельный** `Authorization: Bearer <GUARD_AGENT_TOKEN>`. Если токен не задан серверу, `/api/agent/*` возвращает 503. Неправильный токен — 401. Basic не заменяет токен агента. Не передавайте токены в URL и не сохраняйте в Git.

## Маршруты оператора

| Метод и путь | Назначение | Успех |
|---|---|---|
| `GET /healthz` | Жив ли HTTP-сервер; не проверка камеры | 200 |
| `GET /api/settings` | Полная политика с текущей revision | 200 |
| `PUT /api/settings` | Заменить политику с optimistic concurrency | 200 |
| `GET /api/status` | Состояние RTSP, политики, агента, журнала | 200 |
| `POST /api/preview` | Включить/отключить просмотр, опционально сменить источник | 200 |
| `POST /api/arm` | Сохранить желаемый режим охраны | 200 |
| `POST /api/player/events` | Записать разрешённое событие плеера | 204 |
| `POST /api/stream/offer` | Согласовать WebRTC-сеанс по SDP | 200 |
| `DELETE /api/stream/viewers/{id}` | Освободить WebRTC-сеанс | 204 |
| `GET /api/events` | Найти события | 200 |
| `GET /api/events/export` | Скачать JSONL | 200 |
| `GET /api/events/stream` | SSE со статусом и новыми событиями | 200 |

## Настройки

Пример ответа `GET /api/settings` и тела `PUT /api/settings`:

```json
{
  "revision": 1,
  "camera_name": "Камера 01 · Luckfox",
  "source": "rtsp://95.163.220.75:8554/mystream",
  "preview_enabled": true,
  "armed": false,
  "confidence": 0.5,
  "dwell_seconds": 30,
  "cooldown_seconds": 60,
  "roi": {"enabled": false, "x": 0.15, "y": 0.12, "width": 0.7, "height": 0.76}
}
```

Перед PUT получите свежую политику и отправьте **все поля**. Это полная замена, не PATCH. Клиент отправляет полученную revision; сервер после записи увеличивает её и возвращает обновлённый объект. Устаревшая revision — 409. Ошибка валидации — 400, диска — 500.

`camera_name`: 1–120 байт, `source`: RTSP/RTSPS-адрес до 2048 байт с узлом, confidence 0.05–0.99, dwell 1–3600 с, cooldown 1–86400 с. Координаты ROI 0–1, ширина/высота не меньше 0.01, прямоугольник целиком внутри кадра. Disabled ROI также должен иметь корректные границы.

Полный source в settings может содержать пароль RTSP. Он предоставляется только операторскому API и авторизованному агенту. Публичный для интерфейсных статусов адрес в `/api/status` очищен от userinfo/query/fragment.

## Просмотр и режим охраны

Запуск/смена источника:

```http
POST /api/preview
Content-Type: application/json

{"enabled":true,"source":"rtsp://95.163.220.75:8554/mystream"}
```

Остановка: `{"enabled":false}`. Ответ: `{"settings":{...},"status":{...}}`. Изменение желаемого просмотра сохраняется даже при недоступной камере; ошибка сети отражается в `status.stream.error`, чтение повторяется автоматически. Успешный HTTP 200 здесь не означает получение кадров.

`POST /api/arm` принимает `{"armed":true}` или `{"armed":false}` и возвращает обновлённые settings. Это желаемый режим; подтверждение проверяйте через `agent_online`/`policy_applied` статуса.

`POST /api/player/events` принимает `{"type":"player_stalled"}`. Разрешены `player_stalled`, `player_error`, `player_reconnected`, `snapshot_created`, `rtsp_timeout`, `rtsp_refused`, `rtsp_unauthorized`, `codec_unsupported`. Произвольный текст не принимается, сообщение выбирается сервером. Одинаковый тип ограничен одним событием за 30 с; повтор тоже возвращает 204.

## WebRTC

1. Браузер создаёт RTCPeerConnection без STUN/TURN и recvonly video transceiver.
2. Создаёт offer, устанавливает LocalDescription и дожидается ICE gathering complete.
3. Посылает `POST /api/stream/offer`:

```json
{"type":"offer","sdp":"v=0\r\n...полное SDP браузера..."}
```

4. Ответ:

```json
{"id":"32-символьный-hex-id-сеанса","type":"answer","sdp":"v=0\r\n...SDP Pion..."}
```

5. Браузер вызывает setRemoteDescription с type/sdp и отображает полученный MediaStream. Видеоданные идут по SRTP, не через тело HTTP-ответа и не через SSE.
6. При закрытии плеера отправляет `DELETE /api/stream/viewers/<id>`.

ID в примере условный: реальный ID — 32 hex-символа случайных байтов. До готовности RTSP, при превышении 8 зрителей или ошибке согласования возвращается 503 с понятным текстом. Удаление отсутствующего корректного ID повторяемо и тоже возвращает 204. Ошибки формата SDP — 400 до передачи в медиадвижок, неподходящий offer при согласовании — 503.

Согласуется только видео H.264 или H.265 в зависимости от реального RTSP-источника. В payload offer не передаётся адрес камеры, он берётся из сохранённой политики. Если offer не содержит кодека/профиля источника, ответ — HTTP 422 с JSON `{"error":"описание", "code":"codec_unsupported"}`; временная неготовность источника — 503. Клиент должен предложить принимающий video track (`recvonly` или `sendrecv`). Перекодирование отсутствует. Peer, не завершивший соединение за 20 с, удаляется автоматически. Сетевые ICE-адреса собираются с тайм-аутом 8 с. Для браузера на том же ПК доступны loopback-candidates; внешние сервисы согласования не вызываются.

## Статус

`GET /api/status` содержит:

- `started_at`, `server_time` — UTC-время процесса/сервера;
- `camera_name`, очищенный `source`, `preview_enabled`, желаемый `armed`, `policy_revision`;
- `agent_enabled` — задан ли серверный токен агента;
- `agent_online` — телеметрии меньше 15 с;
- `policy_applied` — свежий агент подтвердил ту же revision и armed;
- `telemetry` — последнее полученное сообщение либо null; даже устаревшее сохраняется для диагностики, UI не показывает его как актуальное;
- `stream` — встроенный видеодвижок;
- `journal_size` — число записей в текущем индексе.

Поля stream:

| Поле | Смысл |
|---|---|
| `registered` | Источник включён в движке |
| `online` | RTSP DESCRIBE/SETUP/PLAY прошли успешно |
| `has_producer` | RTP-пакет приходил в последние 10 с при online |
| `state` | `connecting`, `streaming`, `retrying`, `stopped` |
| `bytes` | Суммарные входящие RTP-байты текущего источника |
| `bitrate_kbps` | Скорость между снимками; null до двух подходящих измерений |
| `codecs` | Кодек выбранного RTSP-источника: H.264 или H.265; это ещё не подтверждение декодирования браузером |
| `error` | Безопасное диагностическое сообщение при ошибке |
| `viewers` | Текущее количество WebRTC-сеансов |
| `last_packet_at` | UTC последнего RTP-пакета |
| `frame_count`, `input_fps` | Завершённые RTP-видеокадры и скорость между серверными измерениями; это ещё не декодированные кадры браузера |
| `last_frame_at` | UTC последнего завершённого RTP-видеокадра |
| `rtp_packets`, `rtp_missing` | Принятые RTP-пакеты и пропуски sequence number, сообщённые RTSP-клиентом; не процент потерь Wi-Fi |
| `rtp_parse_errors` | Ошибки разбора входящих RTP-пакетов |
| `incomplete_frame_events` | События разрыва/незавершённости входного кадра |
| `queue_resets` | Очистки переполненной очереди просмотра |
| `checked_at` | Время снимка |

online не является подтверждением браузерного декодирования. LIVE/FPS измеряет сам браузер по кадрам. Монитор сервера обновляет снимок каждые 3 с, SSE передаёт его каждые 2 с.

## Журнал REST/SSE

`GET /api/events?limit=200&level=warning&source=stream&q=RTSP&after=10` возвращает `{"items":[...]}`. Все фильтры необязательны. level: info/warning/error, source: system/operator/stream/browser/detector. q — подстрока сообщения/типа без учёта регистра. after — ID строго больше указанного. limit по умолчанию 200, максимум 5000; значения вне диапазона заменяются значением по умолчанию. Сортировка от новых к старым.

Событие:

```json
{"id":11,"time":"2026-11-05T12:00:00Z","level":"warning","type":"DWELL_TIMEOUT","source":"detector","message":"Человек долго находится в зоне","track_id":4,"confidence":0.87}
```

`GET /api/events/export` принимает те же фильтры, принудительно использует лимит 5000 и отдаёт `application/x-ndjson`, attachment `guard-events.jsonl`.

`GET /api/events/stream?after=11` поддерживает Last-Event-ID. Без курсора начинает с текущего последнего события; историю сначала загрузите REST-запросом. Формат:

```text
event: status
data: { ... Status ... }

id: 12
event: journal
data: { ... Event ... }
```

Status не изменяет Last-Event-ID. Браузерный EventSource восстанавливает соединение автоматически. Если пропущено больше окна индекса, перезагрузите журнал REST-запросом. Журнал сохраняется между перезапусками, видео через SSE не передаётся.

## API агента

| Метод и путь | Вход | Результат |
|---|---|---|
| `GET /api/agent/policy` | Bearer token | Полный Settings |
| `POST /api/agent/events` | Event | 201, Event с ID/временем сервера |
| `POST /api/agent/telemetry` | Telemetry | 204 |

Минимальное событие:

```json
{"level":"warning","type":"DWELL_TIMEOUT","message":"Track 4 превысил время пребывания","track_id":4,"confidence":0.87}
```

level — info/warning/error; type до 80 байт, message до 2000 байт, confidence 0–1, track_id неотрицательный. source принудительно `detector`. ID и time клиента не используются как доверенные значения.

Телеметрия:

```json
{"model":"YOLOv8n INT8","fps":12.4,"inference_ms":62.5,"persons":1,"vehicles":1,"armed":true,"policy_revision":7}
```

Числа в примере — иллюстрация контракта, а не встроенная демонстрация. Агент должен посылать фактические показатели. received_at сервер ставит сам. Если агент не применил режим/revision, он должен сообщить текущую реально применённую версию, а не копировать желаемую в подтверждение.

### Подключение из Python-детектора

Сервер остаётся на Go. Этот фрагмент — клиент для уже существующего Python-кода:

```python
import json, os, urllib.request

ADMIN_URL = os.environ.get("GUARD_URL", "http://127.0.0.1:8080")
TOKEN = os.environ["GUARD_AGENT_TOKEN"]

def call(path, payload=None):
    body = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        ADMIN_URL + "/api/agent/" + path,
        data=body,
        headers={"Authorization": "Bearer " + TOKEN,
                 "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=3) as response:
        data = response.read()
        return json.loads(data) if data else None

policy = call("policy")
# Примените policy в реальном детекторе: ROI, таймеры, confidence, armed.
# Только ПОСЛЕ успешного применения сохраните applied_revision/armed.
# Раз в 2–5 секунд отправляйте реальные измерения:
# call("telemetry", {"model": model_name, "fps": measured_fps,
#     "inference_ms": measured_ms, "persons": actual_person_count,
#     "vehicles": actual_vehicle_count, "armed": applied_armed,
#     "policy_revision": applied_revision})
# При подтверждённом событии:
# call("events", {"level":"warning", "type":"DWELL_TIMEOUT",
#     "message":actual_message, "track_id":track_id})
```

Встраивайте HTTP-вызовы в отдельный ограниченный worker/очередь: медленная сеть не должна блокировать цикл обработки камеры. При недоступности API сохраняйте события локально и повторяйте отправку с backoff. Контракт не предусматривает idempotency key, поэтому повтор POST после неопределённого сетевого результата может дать дубликат. Для надёжной доставки в следующей версии добавьте agent event ID и серверную дедупликацию.

При агенте на плате используйте LAN-IP компьютера и явно настройте пароль оператора/токен/прослушивание интерфейса. При агенте на том же ПК достаточно localhost. Адрес платы и адрес Go-сервера — разные адреса.

## Коды ошибок

400 — некорректный вход; 401 — неверная авторизация; 403 — посторонний Origin/Host; 404 — неизвестный путь; 409 — устаревшая policy revision; 500 — внутренняя/файловая ошибка; 503 — API агента отключён или медиасеанс ещё не может быть создан. Формат/текст ошибки предназначены для оператора, stack trace и RTSP-пароли в ответы не включаются.
