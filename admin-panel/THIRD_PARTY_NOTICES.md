# Архитектурный ориентир и библиотеки

## Smart-Catch-Up

По просьбе пользователя изучена ветка [dev репозитория Timur1414/Smart-Catch-Up](https://github.com/Timur1414/Smart-Catch-Up/tree/dev), коммит `570a40bc6f97bdaab6053959d00213b17e4dd1af`.

Изучены `cmd/api_gateway/main.go` и модули notification в domain/repository/usecase/delivery. Использован принцип слоёв, явного внедрения зависимостей и сборки приложения в cmd. Код проекта не копировался. Для локальной панели выбран один сервис, файловый журнал и стандартный net/http вместо распределённого набора PostgreSQL/Redis/gRPC. Интерфейсы зависимостей находятся у потребителя в usecase.

## Библиотеки встроенного видеотракта

| Модуль | Версия | Назначение | Лицензия |
|---|---|---|---|
| [gortsplib/v5](https://github.com/bluenviron/gortsplib) | v5.6.7 | RTSP/RTSPS, SDP, RTP по TCP | MIT |
| [mediacommon/v2](https://github.com/bluenviron/mediacommon) | v2.9.6 | Чтение HEVC SPS profile/tier/level | MIT |
| [pion/sdp/v3](https://github.com/pion/sdp) | v3.0.20 | Проверка кодека и направления SDP | MIT |
| [pion/webrtc/v4](https://github.com/pion/webrtc) | v4.2.26 | ICE, DTLS, SRTP, WebRTC | MIT |
| [pion/rtp](https://github.com/pion/rtp) | v1.10.6 | Представление RTP-пакетов | MIT |
| [pion/rtcp](https://github.com/pion/rtcp) | v1.2.20 | RTCP, запрос ключевого кадра PLI | MIT |

Полный перечень транзитивных зависимостей закреплён в `go.mod`, контрольные суммы — в `go.sum`. Лицензии модулей доступны в их исходниках, загруженных Go, и в `dist/THIRD_PARTY_LICENSES.txt` готовой поставки. Эти Go-пакеты компилируются внутрь приложения, не требуют запуска отдельных процессов.

Интерфейс панели использует локальные ресурсы без CDN, аналитики и внешних шрифтов. Видео открывается в штатной странице отдельно запущенного go2rtc. Для SSH-консоли добавлены библиотеки ниже.

## SSH-консоль

- golang.org/x/crypto/ssh v0.57.0 — SSH-клиент и known_hosts, BSD-3-Clause; существующая Go-зависимость стала прямой.
- github.com/gorilla/websocket v1.5.3 — существующая библиотека WebSocket, BSD-2-Clause.
- @xterm/xterm 6.0.0 — эмулятор терминала, MIT. JS/CSS и LICENSE сохранены в web/assets/vendor/xterm.
- @xterm/addon-fit 0.11.0 — подгонка размера терминала, MIT. LICENSE-addon-fit сохранён рядом.

Пакеты xterm скачаны из официального npm registry и проверены по SHA-512 (dist.integrity). Для работы админки npm и интернет не требуются: ресурсы встроены через go:embed.