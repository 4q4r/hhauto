# HHAUTO — полный план (v1.1, 2026-09-24)

> Статус: план утверждён в диалоге (сессия оркестрации + grilling, 3 раунда).
> Режим: Plan mode — файл создан в `/home/whoami/.opencode/plan/hhauto-plan.md`;
> первым действием при выходе из Plan mode скопировать в проект как `docs/PLAN.md`.
> Реализация требует переключения из Plan mode на исполняющего агента.

---

## 1. Цель и Definition of Done

Go CLI-утилита **hhauto** — полностью автоматизированная система поиска работы на hh.ru:
настроил аккаунты/куки/прокси/профиль → `hhauto run` → система сама ведёт воронку **до собеса**.

**DoD (проверяемо):** ежедневно до 200 откликов/аккаунт с каскадом фильтров и ИИ-скорингом,
6 автоподнятий/день, тесты решаются, чаты отвечаются из фактов пользователя, контакты
работодателей собираются, email-дубли откликов отправляются, вебхуки шлют события,
внешние тесты детектятся. Вмешательство человека — только: (а) планирование собесов,
(б) ответы на флаговые вопросы (факт отсутствует в резюме/profile.md), (в) реакция на
техпаузы (капча исчерпана, разлогин). Все три точки логируются в консоль и уходят в вебхуки;
автоматика останавливается только для затронутого чата.

**Платформа:** один бинарник, Go, CGO_ENABLED=0, 6 целей (linux/darwin/windows × amd64/arm64),
goreleaser, лицензия **MIT**. Конфиг — TOML. Работа через **веб-API сайта hh.ru по кукам**
(публичный API закрыт для соискателей 15.12.2025 — подтверждено несколькими источниками).

---

## 2. Зафиксированные решения (все раунды grilling)

| # | Решение |
|---|---|
| 1 | Квоты без консервативных лимитов: дефолт **200 откликов/день на аккаунт** (профили делят лимит аккаунта), **автоподнятие 6/день** (каждые 4ч ± конфиг-джиттер — максимум серверного таймера). Всё конфигурируемо |
| 2 | Паузинг гибкий: окно активности, режим распределения (адаптивный rate = квота/окно, равномерный, burst), ночные паузы — тумблеры. Ramp-up удалён |
| 3 | Капча: **только платные API-сервисы**, мультипровайдер: 2 протокольных адаптера (2captcha-совместимый, Anti-Captcha-совместимый + CapMonster Cloud), каждый провайдер = свой base_url+ключ; аккаунт → failover-цепочка; провайдеры шарятся между аккаунтами. Исчерпание → пауза аккаунта + вебхук |
| 4 | ИИ: 3 диалекта — `openai-chat` (OpenAI/OpenRouter/Ollama/vLLM/GLM/DeepSeek/xAI/Gemini-compat), `openai-responses`, `anthropic-messages`. Per-task: системный промпт (builtin → TOML override), модель, reasoning-effort, температура |
| 5 | Источник фактов ИИ — **только резюме (авто-выгрузка) + profile.md** (глобальный свободный Markdown, опциональный per-account override). Чего нет — не выдумывается: флаг «ответить вручную или дописать в профиль» |
| 6 | dry-run — только явный флаг; по умолчанию **всё отправляется сразу** (отклики, письма, тесты вкл. открытые ответы, чат-ответы) |
| 7 | Наблюдение: CLI + опциональные вебхуки: raw JSON (HMAC, retry+backoff, proxy_ref на эндпоинт, фильтр событий) + пресеты `telegram`, `discord` |
| 8 | Аккаунты — **без ограничений**; прокси опционален, **может разделяться** между аккаунтами; per-account cookie jar/UA. Все аккаунты параллельно, внутри аккаунта — последовательно |
| 9 | Автолив (WF7) и автоподогрев (WF9) — **выключены по умолчанию**, включаются конфигом |
| 10 | Остановки автоматики (chat-level): (а) собес обнаружен, (б) оффер, (в) флаг-вопрос; техпаузы — аккаунт-level. Лог в консоль по всем |
| 11 | ИИ-бюджет по умолчанию безлимит (ручка-лимитер остаётся); security-инварианты сохранены: TLS неотключаем, куки в keyring/encrypted-file, redact логов, kill-switch, fail-closed прокси |
| 12 | Бюджет ИИ и MCP: отвечать «сколько потрачено» — через `hhauto ai status` |
| 13 | Автоправки резюме hh утилитой — **исключено** |
| 14 | TUI — не делаем (CLI + вебхуки достаточно) |

---

## 3. Архитектура

Модульный монолит, single binary:

```text
cmd/hhauto/            # только flags → cli.Execute()
internal/
  cli/                 # cobra-команды, --json, --dry-run, --config
  config/              # TOML: загрузка, ${ENV}-подстановка, дефолты, валидация fail-fast
  logging/             # slog + обязательный redact-хелпер (hhtoken/XSRF/Cookie/Authorization)
  proxy/               # реестр прокси → dialer (socks5 RFC1929 / http(s) CONNECT), fail-closed
  cookies/             # Store-интерфейс: Load/Save/Delete; keyring/encrypted-file реализация
  hhapi/               # клиент веб-API на аккаунт: jar, XSRF (_xsrf + X-XSRFToken),
                       #   UA+client hints из UA, retry/backoff, типизированные ошибки,
                       #   лимитеры (глобал+аккаунт), endpoints/ (search, resumes, apply,
                       #   chatik, employer, reviews, counts), endpoints переопределяемы
  captcha/             # протокольные адаптеры (2captcha-compat, anticaptcha-compat),
                       #   failover-цепочки на аккаунт, base_url+key на провайдера
  ai/                  # Runner: PromptRegistry(embed→TOML) → Guard(инъекции) → Cache(SQLite)
                       #   → Budget → Provider(openai-chat|openai-responses|anthropic-messages)
                       #   → Validator(jsonschema + детерминированные пост-чеки) → Ledger
  store/               # modernc.org/sqlite, WAL, миграции expand-only (PRAGMA user_version)
  scheduler/           # собственный: TZ, окна ЧЧ:ММ, дни, джиттер персистится, Clock-инъекция
  queue/               # типизированная очередь, дедуп, приоритеты, graceful drain
  engine/              # супервизоры аккаунтов (параллельно), внутри аккаунта последовательно
  webhook/             # raw JSON (HMAC, retry, proxy_ref, фильтр событий) + пресеты telegram/discord
  profile/             # авто-выгрузка резюме + profile.md (глобал + per-account override)
  review/              # человеко-контур: очередь флагов, CLI resolve, вебхук-уведомления
  audit/               # аудит действий, счётчики, воронка
  features/            # autolift, multiapply, relevance, emplrating, testsolve, chatops,
                       #   interview, contacts, warmup, emaildup, resume-visibility
```

Принципы: зависимости вниз (`cli→engine→features→{hhapi,store,ai}`); вся сеть в
`hhapi/ai/proxy/webhook`; время и случайность инъецируются; состояние только в store;
маленькие интерфейсы у потребителя.

---

## 4. Стек и зависимости (верифицировано Exa 24.09.2026)

Go ≥1.26 (floor из chromedp v0.16). Проверенные: cobra v1.10.x, pelletier/go-toml/v2 v2.4+,
modernc.org/sqlite v1.59+ (pure Go, WAL), golang.org/x/net v0.58+ (SOCKS5 RFC1929),
x/time/rate, wneessen/go-mail v0.8+, chromedp v0.16+ (внешние формы, требует локальный
Chrome — фича opt-in), santhosh-tekuri/jsonschema/v6, agnivade/levenshtein (fuzzy-детект
инъекций), 99designs/keyring (OS keyring + encrypted-file fallback), x/crypto
(argon2id, chacha20poly1305), oklog/ulid.

**Запрещены**: BurntSushi/toml (unmaintained), robfig/cron (мёртв), любые cgo-пакеты.
Новая зависимость — только через решение оркестратора с Exa-проверкой статуса.
Сборка: `-trimpath -ldflags "-s -w -X main.version=…"`, CI-гейт CGO_ENABLED=0 на все 6 целей.

---

## 5. Конфигурация (TOML) — структура

Полный репрезентативный пример — в отчёте backend-агента (M1 материализует его в
`hhauto config template`). Секции:

- `[general]` data_dir, timezone, dry_run=false
- `[storage]` path, busy_timeout_ms
- `[log]` level/format/file, redact_secrets=true (валидатор запрещает false)
- `[api]` kind="web"|"developer"; `[api.endpoints]` базовые URL (site, chatik, search_page);
  `[api.endpoints.override]` — переопределение ЛЮБОГО эндпоинта (страховка от дрейфа неофициального API)
- `[captcha.<id>]` protocol="2captcha"|"anticaptcha", base_url, key_env, per-task-таймауты;
  аккаунт: `captcha_chain = ["cap1","cap2"]` (failover по порядку; провайдеры шарятся)
- `[proxy.<id>]` type socks5|http|https, host, port, user, pass_env (пароли — только env/vault);
  аккаунт `proxy_ref` опционален, шаринг разрешён
- `[accounts.<id>]` display_name, cookie_file, proxy_ref?, user_agent, headers, rps,
  daily_apply_cap (дефолт 200), captcha_chain, profile_file? (override), concurrency
- `[ai]` daily_budget (0=безлимит); `[ai.providers.<id>]` dialect="openai-chat"|"openai-responses"|"anthropic-messages", base_url, key_env;
  `[ai.models.<id>]` model, reasoning_effort, max_output_tokens, temperature, fallback;
  `[ai.tasks.<task>]` model_profile, prompt: inline|file, cache_ttl; `[ai.pricing]` (опц.)
- `[features.*]` тумблеры всех фич (autolift: enabled=true, per_day=6, jitter="20-40m",
  respect_4h_timer=true; multiapply: ai_filter=true, min_match_percent=60; testsolve;
  chatops (auto_leave_rejected=false*, warmup=false* по умолч.); interview; contacts; emaildup=false*)
- `[pacing]` active_window="08:00-22:00", mode="adaptive"|"uniform"|"burst", night_pause=true,
  min_delay/jitter — всё конфигурируемо
- `[webhook.<id>]` url, preset="raw"|"telegram"|"discord", secret_env (HMAC), proxy_ref?,
  events=[...], retry
- `[runs.<id>]` профили мультиотклика (всё из настроек автоотклика hh):
  account, resume_id, query (OR/AND/NOT/кавычки/скобки), search_mode=exact|normal|wide,
  max_per_day, min_match_percent, окна (tz, time[], days[]), filters{salary, experience,
  schedule/remote, travel_time, employment, industries, areas, labels}, exclude_words,
  company_blacklist/whitelist (+whitelist_bypass), exclude_agencies,
  employer_rating{min_avg, min_recent, recent_days="90"|N, no_reviews_policy},
  ai_filter_overrides, letter_template
- `[review]` notify (console=true, webhook), sla_hours
- `[forms]` внешние тесты: allowed_domains, mode="draft_only"|"headless", submit_policy
- `[email]` smtp (pass_env), from, templates, limits

Валидация fail-fast: ссылочная целостность, «0 = отключено», отрицательные = ошибка,
неизвестный proxy_ref/captcha_ref = ошибка старта (никаких тихих fallback'ов).

---

## 6. Хранилище

SQLite (modernc, WAL, busy_timeout, однописательник через мьютекс store). Таблицы
(DDL — в отчёте backend-агента, миграция 0001): schema_migrations, accounts, resumes,
vacancies_seen (UNIQUE account+vacancy — дедуп), applications (dedup_key UNIQUE,
статусы incl. dry_run/unknown), chats (state: active/rejected/left/escalated/warmup_due,
rejection_confidence), messages (UNIQUE message_id), contacts (UNIQUE account+employer+source,
emails питают emaildup), tests (hh_internal|external_form), interview_links, ping_queue,
ai_decisions (полный аудит: модель, prompt_ver, usage, cost, injection_suspected),
schedules (next_run персистится), feature_state, review_items, employer_ratings
(score_all, score_recent, период), metrics (key+day), intents (write-ahead идемпотентность),
captcha_events, webhook_outbox, profile_facts. Бэкап: `hhauto backup` = VACUUM INTO, 7 копий.

**Идемпотентность**: intents пишутся ДО каждого сайд-эффекта; ключи: отклик
(account,vacancy_id), сообщение (account,chat_id,message_id), пинг (chat_id,stage),
email (employer_id,vacancy_id), поднятие (resume_id,date_window), тест (account,test_id) —
одноразовый. AMBIGUOUS (обрыв после отправки) → только RECONCILE чтением состояния;
авто-повтор запрещён. POST без идемпотентного ключа не ретраится (chatik send несёт
idempotencyKey — ретраится безопасно).

---

## 7. Воркфлоу (детальные деревья — в отчёте workflow-агента)

- **WF1 Автоподнятие**: 6/день (4ч-таймер + джиттер), verify чтением updated_at, 302→капча → captcha-цепочка
- **WF2 Дискавери→фильтры→отклик**: источники — «N вакансий» резюме (`applicantSuitableVacancyByResume`,
  `/shards/vacancy/counts`) и поиск; каскад: нативные фильтры → exclude-слова (название+описание) →
  ЧС/БС → рейтинг работодателя (avg + за N дней из `last3MonthsReviewsInfo`-виджета, no_reviews_policy)
  → дедуп → ИИ-скоринг (порог) → квота дня → предфильтр осуществимости (resumeInconsistencies,
  acceptAutoResponse, chatWritePossibility, letterRequired) → отклик (+сопроводительное ИИ) →
  приоритизация по online-статусу HR и politeness-метрикам → вебхук/лог
- **WF3 Входящие**: поллинг (+опция websocket.hh.ru push) → ИИ-классификация
  (rejection/invitation/test/question/offer/spam) → ветки: автоответ из фактов; тест hh → WF4;
  внешняя ссылка → WF5; собес → извлечение + стоп чата + вебхук; оффер → стоп + вебхук;
  отказ → WF7 (если включён)
- **WF4 Тесты hh**: парсинг → ИИ-ответы (батчами) → сабмит одноразовый (RECONCILE) → лог артефактом
- **WF5 Внешние тесты**: детект ссылок (правило-first по таблице хостов, URL не фетчатся без нужды),
  анти-фишинг (whitelist, password-поле → abort), дефолт draft_only, headless — opt-in (chromedp,
  изолированный контекст без hh-кук)
- **WF6 Контакты**: карточка работодателя + сообщения + email → нормализация (regex-first, ИИ-фолбэк,
  source_quote) → дедуп → хранение → триггер WF8
- **WF7 Автолив** (выкл. по умолч.): детект отказа (структурный статус «Отказ» — первичный сигнал,
  ИИ — вторичный), do_not_leave, humanized delay, template/silent
- **WF8 Email-дубль**: gates (per_employer_once, лимиты), PDF резюме вложением, SMTP STARTTLS обязателен,
  AMBIGUOUS → по Message-ID
- **WF9 Автоподогрев** (выкл. по умолч.): тишина N часов → лестница пингов (24/48/96ч, макс 3),
  list/ai, has_offer гейт
- **WF10 Review**: единая очередь флагов (enum причин), CLI resolve, SLA, вебхук
- **Правило исходящих**: БЕЗ ссылок в откликах/сопроводительных/сообщениях (теневой бан — практика
  пользователей hh-applicant-tool); валидатор + правило в ИИ-промптах
- **Дедуп с нативным автоответчиком hh** (`/shards/autoresponse/statistics`) — не откликаться дважды

---

## 8. ИИ-подсистема

8 задач со строгими JSON-схемами (vacancy_scoring, cover_letter, chat_reply, test_answer,
warmup_ping, message_classification, contact_extraction, link_detection); сквозной контракт
`confidence`/`needs_human`/`needs_human_reason`/`injection_suspected`. Анти-галлюцинация:
used_facts/missing_data/based_on_profile; детерминированные пост-чеки в Go (числа письма ⊆
числа резюме/профиля; regex-гейт запрещённых тем: ЗП, уровни языков, годы опыта по скиллам —
если их нет в источниках). Недоверенные данные (вакансии/чаты/тесты) — только в user-блок со
случайными ограничителями; trusted (профиль/правила/резюме) — в system (= провайдерский
prompt-cache до −90%). max_output_tokens → max_completion_tokens для reasoning-моделей.
Кэш результатов в SQLite (hash task+prompt+model+input). Golden-set регрессии `hhauto ai eval`,
A/B промптов, аудит каждого решения в ai_decisions. Полные тексты 5 системных промптов —
в отчёте ai-агента (встраиваются как дефолты). Дефолтные пороги: скоринг 0.8, классификация
0.75, письмо 0.85, тесты choice 0.75/multiple 0.85, open — тоже авто (dry-run отменяет ревью-режим).

---

## 9. Безопасность и антибан

- Куки = креды полного доступа: 99designs/keyring (OS keyring; fallback encrypted-file
  Argon2id m=19456,t=2,p=1 + XChaCha20-Poly1305); ввод prompt'ом (не аргументами CLI);
  redact в логах неотключаем; детект разлогина → аккаунт paused:needs_relogin (никаких
  повторов auth-цепочки)
- Прокси fail-closed: у клиента аккаунта нет транспорта без прокси; SOCKS5 remote-DNS;
  TLS-верификация неотключаема из TOML (только CLI-флаг + env-подтверждение + баннер)
- Антибан: adaptive pacing (окно/распределение), backoff+circuit breaker на 429/403/капчу,
  kill-switch трёхканальный (сигналы/файл STOP/`hhauto stop`), dry-run флагом,
  деградация лимитера при 429, без ссылок в исходящих
- Мультиаккаунт: шаринг прокси разрешён (решение пользователя); изоляция cookie jar/UA
  per-account сохраняется; тайминговая изоляция расписаний
- ИИ-риски: LLM не имеет права действовать (только структурированная рекомендация; исполняет
  Go-код); PII-инвариант (исходящее только из allowlist-полей профиля); фишинг-гейты внешних
  форм; инъекция → injection_suspected → флаг
- ToS-риски зафиксированы (парсинг запрещён п.4.2.13; «массовые отклики» п.1.20 оферты):
  дисклеймер в README обязателен; квота 200/день = официальный потолок hh

---

## 10. CLI

```
hhauto onboarding                 # мастер: config-скелет, аккаунт, вставка кук (prompt), proxy-тест,
                                  #   profile.md Q&A-помощник, проверка сессии, список резюме
hhauto account add|list|check|remove --yes
hhauto account import --browser chrome | --cookies-file cookies.txt
hhauto run [--once] [--profile ID] [--feature X] [--account ID] [--dry-run]
hhauto status [--json] | funnel [--days N]
hhauto vacancies search --profile ID [--limit N] [--dry-run]
hhauto resume list | lift --resume HASH
hhauto chats list|show|reply|leave --yes
hhauto contacts export --format csv|json
hhauto tests list|show|solve|open
hhauto review list|show|resolve|ignore
hhauto profile edit               # $EDITOR → profile.md (+ валидация фактов)
hhauto ai status|eval|compare
hhauto config validate|template
hhauto doctor                     # сеть, прокси (egress IP), сессии, БД, Chrome, captcha-баланс
hhauto backup | version
```

---

## 11. Гит-источники и provenance

### Код — берём (MIT/Apache/Unlicense), с фиксацией SHA

| Репо | Лицензия | Что берём |
|---|---|---|
| `L0thlorien/hh-submitter` (Go) | MIT | Референс CLI (login/search/apply/chat), хранение сессии, goreleaser-конфиг |
| `rycln/hhraiser` (Go) | MIT | Алгоритм расписания автоподнятия (times+jitter), webhook-уведомления |
| `Zoomish/go-hhru-api` (Go) | MIT | Retry-поведение на 429/503 (точечно) |
| `celloopa/ghosted`, `valentino7504/jobtrack` (Go) | MIT | Паттерны воронки/локальной истории (идеи) |
| `yo61/jobhound` | Apache-2.0 | Event-based модель статусов (идея для схем) |
| `konard/hh-job-application-automation` (JS) | Unlicense | Поведение: эскалация при тестовых вопросах, QA-база, auto-submit флаг |

### Функционал/документация — БЕЗ копирования байтов

| Источник | Лицензия | Что берём |
|---|---|---|
| `Vlad9572324/hh.ru-clicker` → **HH_API_MAP.md** | нет | Карта 476 эндпоинтов — первичный документ спайка M0; код не трогаем |
| `s3rgeym/hh-applicant-tool` (505★, Python) | **Limited Non-Commercial** (запрещает публикацию модификаций) | Код НЕ берём. Поведение/знания: команды (apply-similar, reply-chats, clear-negotiations, call-api, check-proxy), лимит 200/день подтверждён, **мобильный API-транспорт (Android-эмуляция, access/refresh токены) — работающий аварийный fallback (opt-in, по умолч. выкл: основной путь — веб-API по кукам)**, эвристика «ссылки → теневой бан», фрод-эвристики контактов, headless-авторизация с капчей |
| `s3rgeym/hh-ai-responder` (Go) | NOASSERTION | Механика кук (cookies.txt, redirect_host), флоу чистки чатов — читаем, реализуем сами |
| `hhru-bot` (PyPI) | не установлена | CLI-дизайн: --dry-run/--force, import-cookies, data-qa селекторы одним файлом, статус «не знаю» |
| `feder-cr/AIHawk` | AGPL-3.0, archived | Категорически не копируем; анти-паттерны (god-class, селекторы) |
| `Vadim-Matsul/hh-ai-responder` | Other | Избегать полностью |

### Provenance-политика

Каждый заимствованный фрагмент >10 строк — заголовок-комментарий + `docs/provenance.md`
(репо, SHA коммита-источника, лицензия, что изменено); скопированные файлы целиком — в
`NOTICE/THIRD-PARTY.md`. Заимствования фиксируются по конкретному SHA, не по «main».

---

## 12. Скилл-требования к оркестратору (мне)

- Исполнение: `brainstorming` + `build-with-exa` (Exa-only для внешних фактов) +
  два скилла оркестрации: `dispatching-parallel-agents`, `subagent-driven-development`
- Планирование имплементации: `writing-plans`; исполнение плана: `executing-plans`/
  `subagent-driven-development`; каждая фича — `test-driven-development`
- Ревью: `requesting-code-review` + code-reviewer-субагент на каждую задачу; финальный
  whole-branch review на самой сильной модели
- Качество: `quality-gate` (gofmt, go vet, golangci-lint strict, `go test -race -p 2`,
  CGO_ENABLED=0 все 6 целей, govulncheck), `security-scan` перед релизом,
  `verification-before-completion` (никаких «готово» без доказательств прогона)
- Отладка: `systematic-debugging`; коммиты: `git-commit-pro`, branch→PR→squash, никогда в main
- Память: обновление `memory/projects/hhauto.md` после вех и исправлений пользователя
- Живая проверка hh: cloakbrowser non-visual (snapshot/read_page); скриншоты запрещены
- Машинные особенности: GOTMPDIR=/home/whoami/.cache/go-tmp (tmpfs /tmp 7.5G), `-p 2` для
  стабильного `-race`

---

## 13. Протокол субагентов

1. Только тематические агенты (general-purpose/Explore/Plan ЗАПРЕЩЕНЫ): backend-architect,
   ai-engineer, security-architect, workflow-architect, database-optimizer, test-automation-engineer,
   devops-automator, technical-writer, code-reviewer, minimal-change-engineer
2. Каждый диспетч: `brainstorming` первым (с классификацией) + `build-with-exa` (все внешние
   факты через Exa MCP, ретраи на таймауте, URL на каждый факт) + **минимум 3 скилла на выбор
   самого субагента** из полного списка скиллов (перечислить использованные в отчёте)
3. Все субагенты стартуют с **background=true** (пользователь общается с оркестратором
   параллельно); ожидание — только через уведомления, не polling
4. Изоляция: параллельные имплементеры — каждый в своём git worktree; `git add -A` запрещён
   без аудита `git show --stat`
5. Запреты: не трогать файлы вне своего worktree; не диспетчить своих субагентов; долгие
   команды — только БЛОКИРУЮЩЕ с ожиданием вывода в этом же ходе; не пушить/не мержить —
   пайплайн branch→PR→squash ведёт только оркестратор
6. Отчётный контракт: DONE / DONE_WITH_CONCERNS / NEEDS_CONTEXT / BLOCKED + коммиты + одна
   строка о тестах + concerns; полный отчёт — в файл-путь, в ответе — только контракт
7. Ревью-петля: task-reviewer на каждую задачу (вердикты spec+quality), фикс-раунды ≤5
   (1–3 резюм имплементера, 4–5 свежий агент на модель выше), финальный whole-branch review
   на самой сильной модели; миноры — в ledger
8. Модель-селекция: механика — дешёвая, интеграция — средняя, архитектура/финальный ревью —
   самая сильная; указывается явно в диспетче
9. Ледж плана (progress/rulings/parks) ведётся в workspace плана — переживает компакцию
10. Факты hh: субагент не «знает» API из памяти — только M0-фикстуры, HH_API_MAP.md, Exa;
    правка эндпоинтов — только через `[api.endpoints]` конфиг

---

## 14. Роадмап (все этапы, полный скоуп — после M6 система готова)

- **M0 Живой спайк API**: куки-сессия по HH_API_MAP.md — резюме, popup отклика, touch/капча
  (тип капчи!), chatik (+idempotencyKey), рейтинги работодателей, «N вакансий»/counts, email
  работодателя, SSR politeness-метрики; фиксация фактических контрактов в testdata-фикстуры;
  оценка мобильного API как fallback (только документация, не реализация v1)
- **M1 Фундамент**: config+валидация, store+миграции, proxy fail-closed, cookies+keyring,
  hhapi-клиент, logging+redact, doctor, CLI-скелет, билд-матрица, CI
- **M2 Ядро откликов**: WF1 + WF2 (каскад, квота 200, приоритизация, предфильтр осуществимости) +
  captcha-цепочки + review-очередь + intents/RECONCILE
- **M3 ИИ**: ai/ Runner (3 диалекта, промпты, кэш, бюджеты) + relevance + golden set + A/B
- **M4 Чаты**: WF3 классификация/автоответы + WF4 тесты hh + WF7* + WF9* + websocket-опция +
  QA-база обучаемая
- **M5 Обвязка**: WF6 контакты + WF8 email + WF5 внешние формы (draft_only + headless opt-in) +
  resume-visibility + дедуп с нативным автоответчиком
- **M6 Выпуск**: вебхуки (raw+telegram+discord) + воронка/статистика + hardening + README
  (TOC, бейджи, Mermaid, дисклеймер) + goreleaser релиз всех 6 целей

---

## 15. Риски (честно)

- ToS hh: парсинг прямо запрещён (п.4.2.13), «массовые отклики» — основание блока (п.1.20);
  200/день = потолок, работа на пределе повышает внимание антифрода
- Связывание аккаунтов по устройству/TLS возможно на стороне hh (пер-аккаунтный прокси
  снижает, не гарантирует — подтверждено кейсами)
- Часть контрактов веб-API — из реверс-карты (средняя уверенность) → M0 обязателен до M2
- Капча: тип не верифицирован → выбор протокольных адаптеров закрывает большинство сервисов,
  цепочка + failover
- Внешние формы: анти-бот сайтов-форм — главная неизвестная WF5 (draft_only — безопасный дефолт)
- Мобильное API не реализуем в v1 (пользовательское ограничение), задокументирован как fallback
- README-дисклеймер «на свой риск, не аффилировано с hh.ru» — обязателен

## 16. Критерии приёмки каждого milestone

TDD (красный→зелёный), `go vet` + golangci-lint strict 0/0, `go test -race -p 2` зелёный,
CGO_ENABLED=0 сборка всех 6 целей, интеграционные тесты на httptest-фикстурах M0,
код-ревью субагентом (spec+quality) чистое, демонстрация фичи в dry-run на живой песнице
(отдельный тестовый аккаунт), обновление provenance/ledger.
