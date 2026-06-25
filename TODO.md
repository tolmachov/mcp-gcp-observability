# TODO: mcp-gcp-observability

Идеи и недостающие инструменты, выявленные при сравнении с [google-cloud-mcp](https://github.com/krzko/google-cloud-mcp) (TypeScript, 47 инструментов).

---

## Сравнение инструментов

| Категория | mcp-gcp-observability (Go, 24 tools) | google-cloud-mcp (TS, 47 tools) | Вывод |
|-----------|--------------------------------------|----------------------------------|-------|
| **Метрики** | 5: list, snapshot, top_contributors, related, compare | 3: query, list, natural-language | Go-версия **сильнее** — semantic registry, baseline, anomaly classification, SLO breach |
| **Логи** | 7: query, k8s, by_trace, by_request_id, find_requests, services, summary | 3: query, time-range, search | Go-версия **сильнее** — K8s-фильтры, корреляция по trace/request ID, service discovery |
| **Трейсы** | 3: trace_get, trace_list, trace_find_from_logs | 4: get, list, find-from-logs, natural-language | Паритет (кроме natural-language) |
| **Ошибки** | 3: errors_list, errors_get, errors_trends | 3: list, get-details, analyse-trends | Паритет |
| **Профилирование** | 6: list, top, peek, flamegraph, compare, trends | 3: list, analyse, compare-trends | Go-версия **сильнее** — pprof-анализ, flamegraph, peek callers/callees, diff profiles |

---

## ~~1. Профилирование (Cloud Profiler) — новая категория~~ DONE

Реализовано в коммите `11137eb`. 6 инструментов:
- `profiler_list` — листинг профилей с фильтрацией и пагинацией
- `profiler_top` — ранжирование функций по стоимости (как pprof top)
- `profiler_peek` — callers/callees функции (как pprof peek)
- `profiler_flamegraph` — ограниченное поддерево call tree
- `profiler_compare` — сравнение двух профилей, diff profile для drill-down
- `profiler_trends` — отслеживание стоимости функций во времени

Реализация значительно шире исходного плана: pprof-парсинг, LRU-кэш профилей, diff profiles, ambiguity detection, truncation hints, 614 тестов.

---

## 2. Трейсы — недостающие инструменты

Сейчас есть только `trace_get` (получение по ID). В google-cloud-mcp есть ещё 3 инструмента.

### 2.1 `trace_list` — Листинг трейсов

Поиск и листинг трейсов с фильтрацией (аналог `gcp-trace-list-traces`).

**Параметры:**
- `filter` (string, optional) — фильтр (span name, latency, labels)
- `start_time`, `end_time` — временной диапазон
- `limit` (int, default 20)
- `order_by` (string, optional) — сортировка
- `project_id`

**Зачем:** сейчас trace ID нужно знать заранее или получать через `logs_find_requests`. Прямой поиск трейсов по критериям — важный workflow.

### 2.2 `trace_find_from_logs` — Поиск трейсов из логов — DONE

Реализовано: `internal/tools/trace_find_from_logs.go` + `internal/gcpdata/traces_from_logs.go`. Сканирует логи по произвольному фильтру, группирует по trace ID, возвращает отдельные трейсы с объёмом логов, severity, сервисом и сэмплом сообщения. Работает с любым фильтром (в отличие от `logs_find_requests`, только HTTP).

Автоматический поиск трейсов, связанных с определёнными логами (аналог `gcp-trace-find-from-logs`).

**Параметры:**
- `log_filter` (string) — фильтр логов для поиска связанных трейсов
- `start_time`, `end_time`
- `limit`
- `project_id`

**Зачем:** упрощает workflow "нашёл ошибку в логах → хочу посмотреть трейс". Частично покрывается `logs_find_requests`, но тот ищет только HTTP-запросы.

---

## 3. Error Reporting — недостающие инструменты

### 3.1 `errors_trends` — Анализ трендов ошибок — DONE

Реализовано: `internal/tools/errors_trends.go` + `internal/gcpdata/errors_trends.go`. Использует `TimedCounts` из `ListGroupStats`: делит окно на старую/недавнюю половину и классифицирует группы (new/growing/shrinking/disappeared/flat), сортирует по delta (самые ухудшившиеся сверху), даёт summary по категориям. Корреляция с деплоями не реализована (нужна доп. информация о версиях).

Анализ изменения частоты ошибок во времени (аналог `gcp-error-reporting-analyse-trends`).

**Параметры:**
- `time_range_hours` (int, default 168 = 7 дней)
- `service_filter` (string, optional)
- `project_id`

**Ответ:**
- Тренды по группам ошибок (растущие, убывающие, новые, исчезнувшие)
- Корреляция с деплоями (если доступна информация о версиях)
- Top-N ухудшившихся групп

**Зачем:** текущие `errors_list` и `errors_get` показывают только текущее состояние, без динамики.

---

## 4. Nice-to-have идеи

### 4.1 Natural language queries

В google-cloud-mcp есть `query-natural-language` для метрик и трейсов — текст преобразуется в фильтр через внутренний lookup. Можно реализовать аналогичное через semantic registry (уже есть keyword search в `metrics_list`), расширив его до полноценного query builder.

### 4.2 MCP Resources (URI-based navigation) — DONE (частично)

Реализованы URI-шаблоны ресурсов в `internal/server/server.go` (`registerResourceTemplates`):
- `gcp-logs://{project}/recent` — сводка недавних логов — DONE
- `gcp-errors://{project}/groups` — группы ошибок — DONE
- `gcp-traces://{project}/recent` — недавние трейсы — DONE
- `gcp-metrics://{project}/types` — доступные метрики — пока не реализовано

Completer дополняет аргумент `{project}` дефолтным проектом.

### 4.3 Billing инструменты

google-cloud-mcp имеет 10 инструментов для биллинга (анализ расходов, обнаружение аномалий, рекомендации по оптимизации). Это ортогонально observability, но может быть полезно как отдельный модуль.

---

## Приоритеты

1. ~~**Высокий**: Профилирование~~ — **DONE** (6 tools, коммит `11137eb`)
2. ~~**Высокий**: trace_list~~ — **DONE**
3. ~~**Средний**: errors_trends~~ — **DONE**
4. ~~**Средний**: trace_find_from_logs~~ — **DONE**
5. ~~**Низкий**: MCP Resources~~ — **DONE** (logs/errors/traces URI-шаблоны; metrics types ещё нет)
6. **Низкий**: Natural language queries, `gcp-metrics://{project}/types` ресурс, Billing
