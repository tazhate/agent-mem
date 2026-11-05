# amem

[![ci](https://github.com/tazhate/agent-mem/actions/workflows/ci.yml/badge.svg)](https://github.com/tazhate/agent-mem/actions/workflows/ci.yml)

Память между сессиями для AI-агентов: Claude Code, Codex, Gemini CLI, Cursor, OpenCode. Один статический Go-бинарь, одна SQLite, без Node, Python и векторной базы. Замена claude-mem с переносом данных.

Что делает. Хуки пишут события сессии в локальный демон. Observer раз за ход сжимает их в короткие записи через выбранную модель. Recall подсовывает агенту нужное в нужный момент: на старте сессии, на промпт, перед чтением файла с историей, на упавшую команду, после compact. Поиск локальный, p99 18 мс на 108 тыс. записей, так что срабатывает он часто и ничего не ждет.

Концепция и причины решений описаны в [DESIGN.md](DESIGN.md).

## Установка

Готовый бинарь для Linux и macOS (amd64, arm64) лежит в [релизах](https://github.com/tazhate/agent-mem/releases). Или из исходников, нужен Go 1.26:

```bash
go install github.com/tazhate/agent-mem/cmd/amem@latest
amem init
```

Windows не поддерживается: демон работает через unix-сокет и flock.

`amem init` спрашивает четыре вещи:

- **Observer** - чем писать записи. Подписка через CLI (`claude`, `codex`, `gemini`: свой логин, общие лимиты) или API-ключ (`anthropic`, `openai` - это любой OpenAI-совместимый сервер: OpenRouter, Ollama, vLLM, DeepSeek). Ключ хранится ссылкой: `env:NAME`, `cmd:vault kv get ...`, `file:path`.
- **Embedder** - `none` (только полнотекстовый поиск со стеммингом ru/en), Ollama или любой `/v1/embeddings`. Для смеси русского и английского подходит `bge-m3`.
- **Язык** записей.
- **Recall mode**: `on`, `shadow` (считать, что вставить, но не вставлять) или `off`.

Без вопросов то же самое задается флагами:

```bash
amem init --yes --observer claude --embed none --language Russian --recall shadow --runtimes claude-code,codex
```

Хуки ставятся отдельно и снимаются обратно:

```bash
amem install claude-code --dry-run   # показать diff
amem install claude-code
amem install claude-code --uninstall
amem install detected                # все найденные runtime
```

Установщик дописывает только свои записи, чужие хуки и порядок ключей не трогает. Рядом с каждым измененным файлом остается `*.bak-amem-<время>`. Codex после установки один раз попросит подтвердить доверие к новым хукам.

## Перенос из claude-mem

```bash
amem import claude-mem --dry-run
amem import claude-mem
```

Исходная база открывается только на чтение, воркер claude-mem может работать параллельно. Импорт идемпотентный: повторный запуск добавит только новое, поэтому его можно гонять, пока обе системы живут рядом.

При переносе:

- 100+ самодельных типов claude-mem сводятся к семи: `decision`, `bugfix`, `gotcha`, `change`, `discovery`, `preference`, `todo`. Исходный тип сохраняется в `meta.orig_type`;
- наблюдения без живой сессии получают синтетическую сессию на день;
- проекты-worktree (`api/wt-feature`) сливаются с репозиторием;
- проекты без записанного пути становятся `legacy:<имя>` и сливаются сами, когда стартует сессия в репозитории с таким именем.

Chroma не переносится. Векторы MiniLM плохо работают на русском, их пересчитает выбранный эмбеддер.

На базе в 750 МБ импорт занимает около 1.5 минуты.

Порядок переключения:

1. `amem init --recall shadow` и `amem install claude-code`. Неделю amem пишет и считает, но ничего не вставляет.
2. `amem shadow-report` - что он вставил бы.
3. `recall.mode = "on"` в конфиге, плагин claude-mem выключить.
4. Финальный `amem import claude-mem` для догона.

## Команды

| Команда | Зачем |
|---|---|
| `amem search [--project P\|all] [--type T] запрос` | поиск, как его видит агент |
| `amem show 123 456` | полные записи |
| `amem save [--global] [--type T] текст` | ручная заметка; `--global` видна во всех проектах |
| `amem doctor [--probe]` | демон, хуки, провайдер, эмбеддер, spool |
| `amem stats` | счетчики базы |
| `amem watch --preset claude\|codex` | транскрипты для runtime без хуков (или `--subagents-only` для Codex) |
| `amem bench recall\|hook` | задержки на своей базе |

MCP-инструменты для агента: `memory_search`, `memory_get`, `memory_timeline`, `memory_save`, `work_state`. Демон может отдавать их и по streamable HTTP, если в конфиге задан `daemon.http_addr`.

## Файлы

| Путь | Что |
|---|---|
| `~/.config/amem/config.toml` | конфиг, права 0600 |
| `~/.local/share/amem/amem.db` | данные, FTS5-индекс и векторы |
| `~/.local/share/amem/amemd.sock` | сокет демона |
| `~/.local/share/amem/spool.jsonl` | события, пока демон недоступен |
| `~/.local/share/amem/amemd.log` | лог демона |

`AMEM_HOME` переносит все в другой каталог: конфиг, базу, сокет. Так удобно гонять тесты рядом с рабочей установкой. `AMEM_DISABLE=1` выключает хуки в текущем окружении; observer сам ставит его своим CLI-процессам, чтобы не было рекурсии.

## Как это устроено

```text
runtime hook -> amem hook (2-10 мс) -> unix socket -> amemd -> SQLite (WAL)
                    |                                   |-- recall: FTS5 + стемминг + векторы, < 20 мс
                    +-- демона нет: spool + запуск      |-- observer: батч на ход -> LLM -> JSON schema
                                                        +-- embedder: фоном, пауза, пока идут хуки
```

Хук никогда не ломает сессию агента: любая ошибка дает выход 0 и пустой ответ, а событие уходит в spool. Повторная доставка отсекается по id события. Тест `scripts/durability.sh` проверяет это через `kill -9` демона посреди 200 параллельных хуков.

## Разработка

```bash
go test -race ./...
AMEM_HOME=/tmp/amem-dur ./scripts/durability.sh
AMEM_QUALITY_DB=/path/amem.db go test ./internal/recall -run Manual -v   # глазами оценить recall
```
