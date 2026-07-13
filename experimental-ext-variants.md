# Техническое задание: интеграция experimental-ext-variants

## Мотивация

У mcp-gcp-observability 40+ инструментов с подробными описаниями. Это создаёт несколько проблем:

1. **Контекстный бюджет.** Полный `tools/list` занимает тысячи токенов. Для автономных агентов и моделей с маленьким контекстным окном — критично.
2. **Релевантность инструментов.** Агент для мониторинга метрик не нуждается в инструментах профилировщика. Агент для дежурства — не нуждается в инструментах сравнения профилей.
3. **Один сервер, разные клиенты.** Claude Desktop (интерактивное расследование с человеком) и автономный monitoring-бот имеют принципиально разные потребности по глубине описаний и набору инструментов.

[experimental-ext-variants](https://github.com/modelcontextprotocol/experimental-ext-variants) (SEP-2053) — экспериментальное расширение MCP, позволяющее одному серверу предоставлять несколько селектируемых наборов возможностей. Клиент передаёт hints при инициализации, сервер ранжирует варианты и возвращает оптимальный.

---

## Предлагаемые варианты

### `full` — полный (default, stable)

Текущее поведение без изменений. Все инструменты, полные описания.

```
hints:
  useCase: "human-assistant"
  contextSize: "standard"
```

**Инструменты:** все 40+ (logs, errors, traces, metrics, profiler)  
**Целевой сценарий:** интерактивное расследование инцидентов с человеком в Claude Desktop / IDE

---

### `compact` — компактный (stable)

Все инструменты, но описания сокращены примерно вдвое. Убраны: примеры использования, перечисления последующих шагов, рекомендации по workflow. Остаётся: что делает инструмент, ключевые параметры, когда использовать.

```
hints:
  contextSize: "compact"
  useCase: "autonomous-agent"
```

**Инструменты:** все 40+  
**Целевой сценарий:** автономные агенты, API-клиенты, тайтовый контекстный бюджет

---

### `monitoring` — мониторинг (experimental)

Минимальный набор инструментов для автоматического мониторинга и первичной триажировки. Только инструменты, достаточные для ответа на вопрос "что сломалось и насколько серьёзно".

```
hints:
  useCase: "autonomous-agent"
  contextSize: "compact"
```

**Инструменты (10):**

| Категория | Инструменты |
|-----------|-------------|
| Логи | `logs_summary`, `logs_services` |
| Ошибки | `errors_list`, `errors_get` |
| Метрики | `metrics_snapshot`, `metrics_top_contributors` |
| Трейсы | `trace_list`, `trace_get` |
| Профилировщик | `profiler_list`, `profiler_top` |

**Целевой сценарий:** monitoring bot, алертинг-агент, scheduled health checks

---

## Архитектура

### Зависимость

```bash
go get github.com/modelcontextprotocol/experimental-ext-variants/go/sdk@<commit>
```

Библиотека не имеет релизных тегов — пинить на конкретный commit из main.

### Структура внутреннего сервера

Каждый вариант — отдельный `*mcp.Server` с независимой регистрацией инструментов. Все варианты разделяют одни и те же GCP-клиенты и registry.

```
variants.Server (фасад)
├── fullServer    (*mcp.Server) — RegisterAll(s, deps)
├── compactServer (*mcp.Server) — RegisterAll(s, deps, compact=true)
└── monitoringServer (*mcp.Server) — RegisterCore(s, deps)
```

### Изменения в коде

#### `internal/tools/tools.go` — функции регистрации

Ввести понятие `RegistrationMode`:

```go
type RegistrationMode int

const (
    ModeStandard RegistrationMode = iota
    ModeCompact
)
```

Параметризовать все `Register*` функции через `mode`. В режиме `ModeCompact` описание = первое предложение текущего description (обрезать по первой точке).

Добавить `RegisterCore(s *mcp.Server, ..., mode RegistrationMode)` — регистрирует только 10 core инструментов.

#### `internal/server/server.go` — переход на variants.Server

```go
import "github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"

func buildVariantsServer(impl *mcp.Implementation, deps *Deps) *variants.Server {
    fullServer := mcp.NewServer(impl, nil)
    tools.RegisterAll(fullServer, deps, tools.ModeStandard)
    registerResources(fullServer, deps)

    compactServer := mcp.NewServer(impl, nil)
    tools.RegisterAll(compactServer, deps, tools.ModeCompact)
    registerResources(compactServer, deps)

    monitoringServer := mcp.NewServer(impl, nil)
    tools.RegisterCore(monitoringServer, deps, tools.ModeCompact)
    registerResources(monitoringServer, deps)

    return variants.NewServer(impl).
        WithVariant(variants.ServerVariant{
            ID: "full",
            Description: "All GCP observability tools (40+) with complete descriptions. " +
                "Optimized for interactive incident investigation with Claude.",
            Hints:  map[string]string{"useCase": "human-assistant", "contextSize": "standard"},
            Status: variants.Stable,
        }, fullServer, 0).
        WithVariant(variants.ServerVariant{
            ID: "compact",
            Description: "All GCP observability tools with concise descriptions (~50% shorter). " +
                "Optimized for autonomous agents and tight context budgets.",
            Hints:  map[string]string{"useCase": "autonomous-agent", "contextSize": "compact"},
            Status: variants.Stable,
        }, compactServer, 1).
        WithVariant(variants.ServerVariant{
            ID: "monitoring",
            Description: "Core GCP tools only (10): logs_summary, errors_list/get, " +
                "metrics_snapshot/top_contributors, trace_list/get, profiler_list/top. " +
                "For automated monitoring bots and scheduled health checks.",
            Hints:  map[string]string{"useCase": "autonomous-agent", "contextSize": "compact"},
            Status: variants.Experimental,
        }, monitoringServer, 2)
}
```

Заменить в `Run()`:
```go
// было:
if err := s.server.Run(ctx, transport); err != nil { ... }

// стало:
vs := buildVariantsServer(impl, deps)
if err := vs.Run(ctx, transport); err != nil { ... }
```

### Backward compatibility

Клиенты без поддержки variants (нет `variantHints` в `initialize`) получают вариант с наименьшим priority — `full`. Поведение идентично текущему, ничего не ломается.

---

## Ранжирование вариантов

Стандартный `defaultRankingFunc` из библиотеки ранжирует по полю `priority` (меньше = выше). Для нашего случая этого достаточно: `full` (0) > `compact` (1) > `monitoring` (2).

При необходимости можно добавить кастомный `RankingFunc`, который учитывает `contextSize` hint:

```go
.WithRanking(func(ctx context.Context, hints variants.VariantHints, vs []variants.ServerVariant) []variants.ServerVariant {
    cs, _ := variants.HintValue[string](hints, variants.HintContextSize)
    if cs == "compact" {
        // поднять compact выше full
    }
    return vs
})
```

---

## Этапы реализации

### Этап 1 — MVP (минимальный риск)

Цель: добавить варианты не ломая текущее поведение.

1. Добавить зависимость (`go get`)
2. Создать `RegisterCore()` — скопировать 10 регистраций из `RegisterAll()`
3. В `server.go` создать `buildVariantsServer()` с тремя вариантами (compact = те же описания что и full на первом этапе)
4. Убедиться что `full` вариант идентичен текущему поведению
5. Добавить интеграционный тест: подключиться без hints → получить `full` вариант

### Этап 2 — Compact descriptions

1. Ввести `RegistrationMode` и `ModeCompact`
2. Параметризовать все `Register*` функции
3. Вынести логику обрезки описания в хелпер `compactDesc(full string) string`
4. Проверить что compact описания валидны и информативны

### Этап 3 — Тестирование и документация

1. Интеграционный тест: variants/list содержит все три варианта
2. Интеграционный тест: `monitoring` вариант содержит ровно 10 инструментов
3. Интеграционный тест: `compact` вариант содержит все 40+, описания короче
4. Обновить README: раздел "Variants"

---

## Риски

| Риск | Уровень | Митигация |
|------|---------|-----------|
| Библиотека экспериментальная, API нестабильный | Средний | Пинить конкретный commit; при обновлении MCP SDK — перепроверить совместимость |
| Тройная регистрация инструментов — дрейф описаний | Средний | `RegisterAll` параметризован, `RegisterCore` вызывает те же функции |
| Поведение при stdio transport (stateful mode) | Низкий | Библиотека поддерживает stdio, есть примеры `variants-stdio` |
| Хост не поддерживает variants | Нет | Клиенты без hints получают `full` — текущее поведение |

---

## Верификация

```bash
# Сборка
go build ./...

# Unit-тесты
go test ./internal/...

# Интеграционные тесты (требуют GCP credentials)
go test ./test/... -run TestVariants

# Ручная проверка: запустить сервер и посмотреть variants в initialize response
go run . --project=my-project
# В другом терминале: отправить initialize с variantHints и без
```

Для ручного тестирования варианта `monitoring` через Claude Desktop — добавить в `claude_desktop_config.json`:
```json
{
  "mcpServers": {
    "gcp-monitoring": {
      "command": "...",
      "args": ["--variant", "monitoring"]
    }
  }
}
```

*(CLI флаг `--variant` — опциональное удобство: принудительно выбирать вариант, минуя hints-механизм)*
