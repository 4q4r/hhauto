# Third-Party Notices

Зависимости и источники-доноры проекта hhauto. Дата ревизии: 2026-09-24.

## Go-зависимости (прямые, из go.mod)

Лицензии сверены по файлам LICENSE в module cache для точных версий из `go.mod`.

| Пакет | Версия | Лицензия | Назначение |
|---|---|---|---|
| github.com/99designs/keyring | v1.2.2 | MIT | OS-keyring для хранения кук |
| github.com/agnivade/levenshtein | v1.2.1 | MIT | Fuzzy-детект инъекций в промптах |
| github.com/bogdanfinn/tls-client | v1.16.0 | BSD-4-Clause | Браузерный TLS-отпечаток (uTLS) для транспорта hhapi [api].transport=tls-client |
| github.com/oklog/ulid/v2 | v2.1.2 | Apache-2.0 | ULID-идентификаторы |
| github.com/pelletier/go-toml/v2 | v2.4.3 | MIT | Разбор TOML-конфига |
| github.com/santhosh-tekuri/jsonschema/v6 | v6.0.3 | Apache-2.0 | Валидация JSON-ответов ИИ |
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 | CLI-команды |
| github.com/wneessen/go-mail | v0.8.1 | MIT | SMTP для email-дублей |
| golang.org/x/crypto | v0.57.0 | BSD-3-Clause | Argon2id, XChaCha20-Poly1305 |
| golang.org/x/net | v0.59.0 | BSD-3-Clause | SOCKS5 (RFC 1929), прокси-транспорт |
| golang.org/x/term | v0.46.0 | BSD-3-Clause | Ввод секретов без эха |
| golang.org/x/time | v0.16.0 | BSD-3-Clause | Rate-лимитеры |
| modernc.org/sqlite | v1.59.0 | BSD-3-Clause | SQLite без CGO (pure Go, WAL) |

Транзитивные зависимости наследуют лицензии своих проектов (MIT / BSD-3-Clause
/ BSD-4-Clause / Apache-2.0); полный дерево-список: `go list -m all`.
Ключевое транзитивное ядро tls-client: `bogdanfinn/utls` (fork
refraction-networking/utls; LICENSE в module cache — BSD-3, Go Authors),
`bogdanfinn/quic-go-utls` (MIT) и `bogdanfinn/fhttp` (fork net/http из
stdlib Go; апстрим — BSD-3, файла LICENSE в module cache модуля нет —
отмечено как остаточный риск, сверено по происхождению апстрима).

## Доноры идей (код не копировался)

Проекты, чьё поведение и знания изучались при дизайне. Код этих проектов
не входит в hhauto — подробности: [docs/provenance.md](../docs/provenance.md).

| Репозиторий | Лицензия | Что взято |
|---|---|---|
| Vlad9572324/hh.ru-clicker → HH_API_MAP.md | нет (только документ) | Карта эндпоинтов веб-API hh.ru |
| s3rgeym/hh-applicant-tool | Limited Non-Commercial | Поведение: лимит 200/день, «ссылки → теневой бан», мобильный API как fallback (не реализован) |
| s3rgeym/hh-ai-responder | NOASSERTION | Знание механики кук и флоу чатов |
| konard/hh-job-application-automation | Unlicense | Поведение: эскалация при тестовых вопросах, QA-база |
| hhru-bot (PyPI) | не установлена | CLI-паттерны --dry-run/--force |
| L0thlorien/hh-submitter | MIT | Референс CLI и goreleaser-подхода |
| rycln/hhraiser | MIT | Идея расписания автоподнятия с джиттером |
| Zoomish/go-hhru-api | MIT | Поведение retry на 429/503 |
| celloopa/ghosted | MIT | Паттерны воронки (идея) |
| valentino7504/jobtrack | MIT | Паттерны локальной истории (идея) |
| yo61/jobhound | Apache-2.0 | Event-based модель статусов (идея) |

Анти-примеры, не вошедшие намеренно: `feder-cr/AIHawk` (AGPL-3.0, archived —
копирование исключено), `Vadim-Matsul/hh-ai-responder` (Other — избегать).

## hh.ru

hhauto не аффилирован с hh.ru, не одобрен и не поддерживается hh.ru.
Все товарные знаки принадлежат их владельцам.
