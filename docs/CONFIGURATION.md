# Конфигурация hhauto

Полный комментированный пример `hhauto.toml` с пояснениями секций.
Каноничный источник текста шаблона: `hhauto config template` (вывод ниже
идентичен ему); типы и правила валидации: `internal/config/types.go`,
`internal/config/validate.go`. Спека: [PLAN.md §5](PLAN.md).

## Основы

- Файл по умолчанию: `~/.config/hhauto/hhauto.toml`; другой путь — `--config`.
- Плейсхолдеры: `{data_dir}` → `general.data_dir`; `${ENV_VAR}` → переменная
  окружения (**несуществующая переменная прерывает загрузку**); `~` в строках
  → домашний каталог.
- Секреты — только через env (`key_env`, `pass_env`, `secret_env`). Инлайн-ключ
  капчи возможен, но валидатор предупреждает; инлайн-пароль прокси отвергается.
- Валидация fail-fast при старте и в `hhauto config validate`: ссылочная
  целостность (`proxy_ref`, `captcha_chain`, профили моделей), «0 = отключено»,
  отрицательные значения — ошибка; `log.redact_secrets = false` запрещена.
- Каждый блок ниже — самостоятельный пример; строки закомментированы: свежая
  копия шаблона валидна и использует дефолты.

## Полный пример

```toml
# hhauto configuration — full example (docs/PLAN.md §5).
# Every line below is commented out: a fresh copy is valid and uses the
# documented defaults. Uncomment and edit what you need.
# Place: ~/.config/hhauto/hhauto.toml (override with --config)
# Placeholders: {data_dir} expands to general.data_dir; environment
# placeholders (a dollar sign followed by a braced variable NAME) expand to
# env vars — an unset variable aborts the load; a leading "~" in string
# values expands to the home directory.

[general]
# data_dir = "~/.local/share/hhauto"   # stop file, profile.md, backups, logs
# timezone = "Europe/Moscow"           # IANA name; default for schedules
# dry_run = false                       # true = log every mutation, send nothing

[storage]
# path = "{data_dir}/hhauto.db"         # SQLite database (WAL)
# busy_timeout_ms = 5000

[log]
# level = "info"                        # debug | info | warn | error
# format = "text"                       # text (stderr) | json (file)
# file = "{data_dir}/hhauto.log"        # required for json
# redact_secrets = true                 # mandatory: the validator rejects false

[api]
# kind = "web"                          # "web" (cookie surface) is the only v1 mode
[api.endpoints]
# site = "https://hh.ru"
# chatik = "https://chatik.hh.ru"
# search_page = "https://hh.ru/search/vacancy"
# [api.endpoints.override]              # pin ANY endpoint by name (anti-drift)
# settings = "https://hh.ru/applicant/settings"
# resumes = "https://hh.ru/shards/applicant/resumes"
# touch = "https://hh.ru/applicant/resumes/touch"
# resume_pdf = "https://hh.ru/resume/{resume_id}?pdf=1"

# --- captcha: paid API services only (2captcha / anticaptcha protocols) ---
# [captcha.cap1]
# protocol = "2captcha"                 # "2captcha" | "anticaptcha"
# base_url = "https://api.2captcha.com"
# key_env = "HHAUTO_CAPTCHA_KEY_1"      # inline key = also possible, discouraged
# poll_interval_ms = 5000
# timeout_s = 120

# --- proxy: socks5/http/https; optional, accounts may share a proxy ---
# [proxy.socks5-1]
# type = "socks5"
# host = "10.0.0.2"
# port = 1080
# user = "proxyuser"
# pass_env = "HHAUTO_PROXY_PASS_1"      # inline pass is rejected by the validator

# --- accounts: no limit; every account is one hh.ru session ---
# [accounts.main]
# display_name = "Primary"
# user_agent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
# proxy_ref = "socks5-1"                # "" = direct
# rps = 0.5                             # per-account request rate cap
# concurrency = 2
# daily_apply_cap = 200                 # official hh ceiling
# captcha_chain = ["cap1"]              # ordered failover; shared providers
# profile_file = "{data_dir}/profile.md"  # optional per-account override
# [accounts.main.headers]               # extra static headers (NOT User-Agent)
# "Accept-Language" = "ru-RU,ru;q=0.9"

[ai]
# daily_budget_tokens = 0               # 0 = unlimited
# daily_budget_usd = 0
# on_budget_exceed = "needs_human_only" # | "hard_stop"
# [ai.providers.fast]
# dialect = "openai-chat"               # openai-chat | openai-responses | anthropic-messages
# base_url = "https://api.openai.com/v1"
# api_key_env = "HHAUTO_AI_KEY"
# timeout_seconds = 120
# [ai.models.scorer]
# provider = "fast"
# model = "gpt-4o-mini"
# reasoning_effort = ""                 # none|minimal|low|medium|high|xhigh|max
# max_output_tokens = 1024
# temperature = 0.2
# fallback = ""                         # alternative model profile
# [ai.tasks.vacancy_scoring]
# model_profile = "scorer"
# cache_ttl_hours = 24
# [ai.pricing."gpt-4o-mini"]            # USD per 1M tokens (budget accounting)
# input_per_mtok_usd = 0.15
# output_per_mtok_usd = 0.6

[features]
# resume_visibility = false
# websocket_push = false
# emaildup = false                      # WF8: email-duplicate sends (consumed by contacts)
[features.auto_lift]
# enabled = false                       # WF1: 6 lifts/day (hh 4h server timer)
# per_day = 6
# jitter = "20-40m"
# respect_4h_timer = true
[features.multiapply]
# enabled = false                       # WF2: discovery -> filters -> apply
# ai_filter = false
# min_match_percent = 60
# exclude_agencies_default = true
# prioritize_online_hr = true
# dedup_native_autoresponse = true
[features.testsolve]
# enabled = false                       # WF5: external forms, draft_only
# mode = "draft_only"                   # headless requires Chrome in PATH
# submit_policy = "human"
[features.chatops]
# enabled = false                       # WF3/WF4/WF7
# auto_reply_enabled = false
# rejection_min_confidence = 0.8
# auto_leave_rejected = false           # WF7: off by default
# poll_interval = "5m"
# qa_base = false
[features.interview]
# enabled = false                       # interview/offer detection inside chatops
[features.contacts]
# enabled = false                       # WF6
# from_card = true
# from_messages = true
# auto_trigger_emaildup = false

[features.relevance]
# min_match_score = 60

[pacing]
# active_window = "08:00-22:00"
# mode = "adaptive"                     # adaptive | uniform | burst
# night_pause = true
# min_delay_seconds = 0
# jitter = ""

# --- webhooks: raw (HMAC) / telegram / discord presets ---
# [webhook.tg]
# url = "https://api.telegram.org/bot<token>/sendMessage"
# preset = "telegram"
# chat_id = "123456789"
# secret_env = "HHAUTO_WEBHOOK_SECRET"  # raw preset: HMAC key
# proxy_ref = ""                        # optional
# events = []                           # empty = all events
# retry_max = 3

# --- run profiles: the multiapply campaigns ---
# [runs.backend-go]
# enabled = true
# account = "main"
# resume_id = ""                        # "" = first published resume
# query = "golang OR backend"
# search_mode = "normal"                # exact | normal | wide
# max_per_day = 30
# min_match_percent = 60
# letter_template = ""                  # static fallback when the AI is off
# [[runs.backend-go.windows]]
# tz = "Europe/Moscow"
# time = ["09:00", "14:00"]
# days = ["mon", "tue", "wed", "thu", "fri"]
# [runs.backend-go.filters]
# areas = ["1"]                         # Moscow
# experience = ["between1And3", "between3And6"]
# exclude_words = ["стажер", "intern"]
# company_blacklist = []
# company_whitelist = []
# exclude_agencies = true
# min_employer_rating = 0.0
# no_reviews_policy = "allow"           # allow | skip | human
# [runs.backend-go.ai_filter_overrides]
# min_score = 70

[review]
# sla_hours_default = 24

[forms]
# allowed_domains = ["hh.ru", "forms.gle", "docs.google.com", "forms.yandex.ru"]
# mode = "draft_only"
# submit_policy = "human"
# abort_on_password_field = true        # forced true (anti-phishing)
# page_timeout_sec = 20

[email]
# smtp_host = ""
# smtp_port = 587                       # STARTTLS mandatory; 465 = implicit TLS
# username = ""
# pass_env = "HHAUTO_SMTP_PASS"
# from = ""
# subject_template = "Кандидат по вакансии {vacancy_title}"
# body_template = ""
# require_attachment = false
# limits_per_hour = 2
# limits_per_day = 10
# retries = 2

[warmup]
# enabled = false                       # WF9: off by default
# silence_hours = 24
# ladder_hours = [24, 48, 96]
# max_pings = 3
# source = "list"
# ping_list = ["Здравствуйте! Подскажите, пожалуйста, статус по отклику?"]
# active_hours = "10:00-19:00"
# respect_weekend = true

[schedule]
# jitter_minutes = 10
# missed_run_policy = "run_once"        # skip | run_once
# max_catchup_hours = 6
```

## Пояснения секций

### `[general]`
`data_dir` — корень данных (стоп-файл `STOP`, `profile.md`, бэкапы, логи).
`timezone` — IANA-имя, дефолт для расписаний. `dry_run = true` переводит весь
процесс в режим логирования мутаций; боевой режим — по умолчанию.

### `[storage]`
Один SQLite-файл (WAL, busy timeout). Бэкап — `hhauto backup` (VACUUM INTO,
7 ротируемых копий).

### `[log]`
`format = "text"` пишет в stderr, `"json"` — в файл (файл обязателен для json).
`redact_secrets` неотключаем: валидатор отклоняет `false`.

### `[api]`
`kind = "web"` — единственный режим v1 (веб-API по кукам; публичный API закрыт
с 15.12.2025). `[api.endpoints]` задаёт базовые URL; `[api.endpoints.override]`
пинирует **любой** эндпоинт по имени — страховка от дрейфа неофициального API.

### `[captcha.<id>]`
Только платные API-сервисы: `protocol = "2captcha" | "anticaptcha"`
(Anti-Captcha-совместимый покрывает CapMonster Cloud). У каждого провайдера
свой `base_url` и `key_env`; аккаунт ссылается цепочкой
`captcha_chain = ["cap1","cap2"]` — failover по порядку, провайдеры шарятся
между аккаунтами. Исчерпание цепочки → пауза аккаунта + вебхук.

### `[proxy.<id>]`
`socks5` (remote-DNS) / `http` / `https`. Прокси опционален; ссылка —
`proxy_ref` в аккаунте или вебхуке; шаринг между аккаунтами разрешён.
Транспорт без прокси не создаётся (fail-closed); TLS-верификация неотключаема
из TOML.

### `[accounts.<id>]`
Один аккаунт = одна hh.ru-сессия: `cookie_file` (или импорт кук), изолированные
UA и заголовки, `rps` и `concurrency` — локальные лимиты, `daily_apply_cap`
(дефолт 200 — официальный потолок hh), `captcha_chain`, опциональный
`profile_file` (override глобального profile.md).

### `[ai]`
Бюджеты (`daily_budget_tokens`, `daily_budget_usd`, 0 = безлимит) и политика
`on_budget_exceed = "needs_human_only" | "hard_stop"`. Провайдеры — три
диалекта: `openai-chat` (OpenRouter/Ollama/vLLM/GLM/DeepSeek/…),
`openai-responses`, `anthropic-messages`. Модели ссылаются на провайдера
(`provider`), задача — на профиль модели (`model_profile`); `fallback` —
альтернативный профиль. Промпт задачи: builtin → `inline` → `file`.
`[ai.pricing]` — цены USD за 1M токенов для учёта бюджета (`hhauto ai status`).

### `[features.*]`
Тумблеры фич. По умолчанию выключены: `auto_lift`, `multiapply`, `chatops`,
`testsolve`, `interview`, `contacts`, `emaildup`, `warmup` — система без
конфигурации ничего не отправляет. Ключевые ручки:
- `auto_lift`: `per_day = 6`, `jitter = "20-40m"`, уважение серверного 4ч-таймера;
- `multiapply`: `ai_filter`, порог `min_match_percent`, исключение агентств,
  приоритизация онлайн-HR, дедуп с нативным автоответчиком hh;
- `testsolve`: `mode = "draft_only"` (headless требует Chrome в PATH),
  `submit_policy = "human"`;
- `chatops`: `auto_reply_enabled`, порог уверенности отказа,
  `auto_leave_rejected = false` (автолив), `qa_base`;
- `resume_visibility`, `websocket_push` — зарезервированы (websocket — роадмап).

### `[pacing]`
Окно активности `active_window`, распределение `mode = "adaptive" (квота/окно)
| uniform | burst`, `night_pause`, минимальная задержка и джиттер.

### `[webhook.<id>]`
Пресеты: `raw` (JSON + HMAC из `secret_env`, retry, опциональный прокси
на эндпоинт), `telegram`, `discord` (нужен `chat_id` для telegram). `events = []`
= все события. Доставка через outbox — переживает рестарт.

### `[runs.<id>]`
Профили мультиотклика — всё из настроек автоотклика hh: поисковый запрос
(OR/AND/NOT/кавычки/скобки), `search_mode = exact | normal | wide`,
дневные лимиты и пороги, окна (`tz`, `time[]`, `days[]`), фильтры (зарплата,
опыт, график, занятость, отрасли, регионы, exclude-слова, ЧС/БС компаний,
рейтинг работодателя с политикой «без отзывов»). `resume_id = ""` — первое
опубликованное резюме.

### `[review]`, `[forms]`, `[email]`, `[warmup]`, `[schedule]`
- `review`: SLA очереди ручного разбора.
- `forms`: внешние тесты — `allowed_domains`, `mode = "draft_only"`,
  `abort_on_password_field` принудительно `true` (анти-фишинг).
- `email`: SMTP с обязательным STARTTLS (465 = implicit TLS), шаблоны, лимиты
  в час/день, `per_employer_once` (дефолт true).
- `warmup`: автоподогрев (выкл. по умолчанию): тишина N часов → лестница
  пингов, максимум 3, список или ИИ.
- `schedule`: джиттер расписаний, политика пропущенных (`skip | run_once`)
  и потолок догоняющего запуска.

## Проверка

```bash
hhauto config validate   # fail-fast с указанием пути к ошибке
hhauto config template   # этот же шаблон из бинарника
```
