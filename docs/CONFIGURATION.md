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
# transport = "tls-client"              # "tls-client" (default) | "http" (stdlib, debugging)
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

# [ai.jev]                              # T33: Jev (System One) префильтр ПЕРЕД LLM
                                        # Выключен по умолчанию (full AI). Когда включён,
                                        # зачисленные задачи сначала классифицирует Jev-compatible
                                        # эндпоинт: confidence >= порога -> вердикт БЕЗ вызова LLM,
                                        # ниже порога / эндпоинт недоступен / кривой ответ -> LLM.
                                        # Дешёвый быстрый путь не может сломать прогон.
# enabled = false                       # false = режим full AI (раннер без префильтера)
# endpoint = "https://api.typesafe.ai/v1/systemone"  # любой Jev-compatible URL (http/https)
# api_key_env = "TYPESAFE_API_KEY"      # ключ ТОЛЬКО через переменную окружения (fail-fast на старте)
# confidence_threshold = 0.85           # (0,1]; >= порога решает Jev, ниже — эскалация на LLM
# tasks = ["message_classification", "link_detection", "contact_extraction"]
                                        # только эти три задачи Jev-совместимы (классификация без
                                        # генерации текста). Особенности: message_classification и
                                        # link_detection решаются Jev полностью (по одному Choice на
                                        # URL из input.urls); contact_extraction: уверенное «нет
                                        # контактов» решается Jev, уверенное «да» эскалирует на LLM —
                                        # Jev не может извлечь value/normalized/source_quote.
                                        # Вердикты Jev проходят ту же строгую schema-валидацию,
                                        # детерминированные post-checks и пишутся в ledger с
                                        # Model="jev", Source="jev" (LLM-решения: Source="llm").

[features]
# resume_visibility = false
# websocket_push = false
# emaildup = false                      # WF8: email-duplicate sends (consumed by contacts)
[features.auto_lift]
# enabled = false                       # WF1: подъём по серверному таймеру hh (nextTouchAt)
# per_day = 6
# jitter = "20-40m"
[features.multiapply]
# enabled = false                       # WF2: discovery -> filters -> apply
# ai_filter = false
# min_match_percent = 60
# exclude_agencies_default = true
# prioritize_online_hr = true
# dedup_native_autoresponse = true
[features.autoclone]                    # T26/T28: автоклонирование резюме (нативный hh-клон clone_resumes: hh сам копирует контент и ставит parentResumeId; lineage дублируется локально)
# enabled = false
# max_clones_per_resume = 2             # прямых клонов на одно резюме
# max_clone_depth = 1                   # глубина цепочек клон-от-клона (1 = без цепочек)
# autolift = true                       # поднимать клоны вместе с базовыми резюме (auto_lift)
# reapply_mode = "new_only"             # all | new_only | skip_recent_rejections
# skip_rejection_days = 30              # окно свежести отказов для skip_recent_rejections
# finish_timeout_min = 10               # клон висит в not_finished дольше → review + стоп клонов аккаунта
# clone_original_action = "keep"        # T29: keep | hide | delete — судьба ИСХОДНОГО резюме после успешного клона+improve (неудача/неизвестное значение → keep + WARN)
# [features.autoclone.improve]          # T29: ИИ-улучшение полей клона (per-field промпты)
# enabled = false
# fields = ["title", "about", "key_skills", "salary", "experience_description"]
                                        # словарь полей: title, about (О себе), key_skills, salary, professional_roles,
                                        # work_schedule, employment, work_formats, experience_company, experience_position,
                                        # experience_description (по каждому месту работы), experience_dates, education_primary/
                                        # elementary/additional/attestation, citizenship, gender, birthday, relocation,
                                        # work_ticket, driver_license, area, metro, language.
                                        # ИИ-улучшаемые (builtin-промпты): title, about, key_skills, salary, experience_description.
                                        # Даты (experience_dates) — НЕ ИИ-поле: улучшаются только с явным промптом владельца.
# model_profile = ""                    # профиль [ai.models] для задачи resume_improve
# always_ask = []                       # поля, улучшения которых НЕ применяются автоматически: дифф уходит в review
# [features.autoclone.improve.prompts.title]
# inline = "..."                        # свой промпт поля (inline | file — взаимоисключимо); без override — builtin
# file = "prompts/title.md"
# [features.autoclone.run_override]     # точечные override неявного клона-рана (остальное наследует родительский ран)
# max_per_day = 5
# letter_mode = "template_ai"
# letter_template = ""
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
# search_fields = ["name"]              # name | company_name | description; empty = all fields (для "python backend", remote+RU, 2026-09-25: name=28, description=319, все поля=375)
# max_per_day = 30
# min_match_percent = 60
# letter_template = ""                  # static fallback when the AI is off
# letter_mode = "ai_company"            # template | template_ai | ai_company (T26; template* требуют letter_template)
# [[runs.backend-go.windows]]
# tz = "Europe/Moscow"
# time = ["09:00", "14:00"]
# days = ["mon", "tue", "wed", "thu", "fri"]
# [runs.backend-go.filters]
# areas = ["1"]                         # Москва
# professional_roles = ["160"]          # ID специализаций hh (multi)
# metros = ["532"]                      # ID станций метро (multi)
# experience = ["between1And3", "between3And6"]
# schedule = ["fullDay", "remote"]
# employment = ["full", "part"]
# industries = ["7"]                    # ID отраслей (multi)
# work_formats = ["REMOTE", "HYBRID"]   # REMOTE|HYBRID|ON_SITE (регистр важен)
# salary_from = 150000
# salary_currency = "RUR"
# only_with_salary = true               # только вакансии с зарплатой
# labels = ["with_salary"]              # свободные строки (multi); известные — ниже
# part = ["evening", "weekend"]         # temp|evening|weekend (эффект live не подтверждён)
# order_by = "publication_time_desc"    # relevance|publication_time_desc|salary_desc|salary_asc
# search_period_days = 30
# exclude_words = ["стажер", "intern"]
# company_blacklist = []
# company_whitelist = []
# exclude_agencies = true               # label=not_from_agency
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

[browser]
# auto_download = true                  # auto-download the cloakbrowser chromium when no Chrome is found
# humanize = true                       # human-like input in the fillers (typing cadence, micro-delays)
# preset = "careful"                    # careful (default) = stealth launch flags + slower pacing; default = plain
# channel = "auto"                      # auto | free | pro (pro = cached builds only; licensed download out of scope)

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

`transport = "tls-client" | "http"` (дефолт `tls-client`, T24). hh.ru
фингерпринтит TLS-хендшейк (JA3/JA3S): стек Go `crypto/tls` получает
документные поверхности без SSR-узла состояния, поэтому дефолтный транспорт —
`bogdanfinn/tls-client` (pure-Go uTLS) с браузерным профилем, подобранным под
`user_agent` аккаунта: то же семейство (Chrome/Edge → `chrome_*`, Firefox →
`firefox_*`), старший профиль с версией ≤ версии UA (Chrome/138 → `chrome_133`,
Firefox/155 → `firefox_148`). Точного совпадения мажора в библиотеке нет —
WARN в лог при старте; неизвестное семейство UA → дефолтный профиль
библиотеки + WARN. Поведение (куки/персистентность, retry/backoff,
rate-лимит, редиректы с региональным пиннингом и хоп-куками, прокси из
`[proxy.*]` через `SetProxy` — socks5/http/https с кредами в userinfo,
таймаут 30 с на хоп) сохранено; редиректы водятся вручную — у tls-client нет
хуков на хоп. `transport = "http"` — прежний stdlib-транспорт, оставлен как
fallback для отладки (диагностики транспортных различий).

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

> Примечание (R13): импорт сессии — через `hhauto account add --cookies-file`
> (Netscape cookies.txt или JSON-экспорт). Прямая выгрузка cookies из профиля
> браузера (`--browser chrome`) в v1 не реализована — отложена.

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
- `auto_lift` (T36): план подъёмов из шардового `nextTouchAt`
  (пусто → `updated_at + 4h`), один расчёт при старте/после релогина,
  дальше локальный отсчёт 4ч + `jitter` от последнего поднятия (шарда
  между подъёмами не перечитывается);
- `multiapply`: `ai_filter`, порог `min_match_percent`, исключение агентств,
  приоритизация онлайн-HR, дедуп с нативным автоответчиком hh;
- `autoclone` (T26): когда пул вакансий рана исчерпан (всё подходящее уже
  в откликах lineage), создаётся клон резюме (`CloneResume`, живой контракт
  2026-09-25) и запускается неявный ран с унаследованным конфигом. Гейты:
  `max_clones_per_resume` (прямые потомки), `max_clone_depth`,
  `reapply_mode` (`all` — всё заново; `new_only` — только нетронутые
  lineage; `skip_recent_rejections` — кроме отклонённых за
  `skip_rejection_days` по chats.state+last_message_at). Упор в лимит hh
  или клон, застрявший в `not_finished` дольше `finish_timeout_min` →
  review + стоп клонов аккаунта; `dry_run` проходит весь тракт без мутаций.
  В `runs.*` — `letter_mode`: `template` (только шаблон), `template_ai`
  (шаблон как база + ИИ-полировка, фолбэк — сырой шаблон), `ai_company`
  (ИИ под компанию, по умолчанию);
- `autoclone.improve` (T29): после успешного клона ИИ улучшает выбранные
  поля клона — `title`, `about` (О себе), `key_skills`, `salary`,
  `experience_description` (builtin-промпты на каждое поле, override через
  `[features.autoclone.improve.prompts.<field>]`); значения grounded на
  фактах резюме/профиля/правил — числа и даты только из источника
  (детерминированные пост-чеки дают `needs_human` → review). Поле из
  `always_ask` не применяется автоматически: proposed-дифф паркуется в
  review. Применение идёт через редакторский update-эндпоинт по экранам с
  verify-перечитыванием; ошибка валидации hh на поле → один ретрай с
  исправлением формата → skip поля. Каждое применение пишет артефакт
  диффа (было/стало per field) в `resume_clones.improve_diff`. Клоны
  голые не остаются: инвариант — обновляется ТОЛЬКО hash из lineage.
  `clone_original_action` = `keep` (дефолт) | `hide` ( visibility no_one,
  обратимо) | `delete` — судьба ИСХОДНОГО резюме; неудавшийся/неизвестный
  экшен → WARN + keep (fail-safe);
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
Профили мультиотклика: поисковый запрос, `search_fields = ["name" |
"company_name" | "description"]` (пусто = все поля hh), дневные лимиты и пороги, окна (`tz`, `time[]`, `days[]`), фильтры (см. ниже)
и шаблон письма. `resume_id = ""` — первое опубликованное резюме.

#### Язык поисковых запросов hh

Текст `query` передаётся на hh verbatim — доступна вся гибкость официального
языка поисковых запросов (перечень — база знаний hh, статья «Как использовать
язык поисковых запросов»):

| Оператор | Пример | Смысл |
| --- | --- | --- |
| `AND` | `golang AND kubernetes` | оба слова |
| `OR` | `дизайнер OR иллюстратор` | любое из слов |
| `NOT` | `NOT стажёр` | исключить слово/фразу |
| `"фраза"` | `"главный бухгалтер"` | точная фраза |
| `!слово` | `!менеджер` | точная словоформа |
| `"фраза"~N` | `"школа танцев"~5` | до N слов между частями фразы |
| `*` | `менедж*` | все слова с корнем |
| `( )` | `(дизайнер OR иллюстратор) AND ДМС` | группировка условий |
| `NAME:` | `NAME:инженер` | искать только в названии вакансии |
| `COMPANY_NAME:` | `COMPANY_NAME:Сбер` | только в названии компании |
| `DESCRIPTION:` | `DESCRIPTION:"коммерческая недвижимость"` | только в описании |
| `!ID:` | `!ID:123456789` | ID вакансии (восклицательный знак обязателен) |
| `!COMPANY_ID:` | `!COMPANY_ID:816144` | ID компании (восклицательный знак обязателен) |

Регистр имён полей (`NAME:`/`name:`) не важен. Пресетов «Точный/Обычный/
Широкий» нет: гибкость задаётся самим запросом + списком `search_fields`
(живые счётчики «python backend», remote+RU: name=28, description=319,
все поля=375).

#### `[runs.<id>.filters]` — нативные фильтры hh (полное покрытие)

| Ключ | Param hh | Значения |
| --- | --- | --- |
| `areas` | `area` (multi) | ID регионов hh |
| `professional_roles` | `professional_role` (multi) | ID специализаций (например `160` = DevOps) |
| `metros` | `metro` (multi) | ID станций метро |
| `experience` | `experience` (multi) | `noExperience`, `between1And3`, `between3And6`, `moreThan6` |
| `schedule` | `schedule` (multi) | `fullDay`, `shift`, `flexible`, `remote`, `flyInFlyOut` |
| `employment` | `employment` (multi) | `full`, `part`, `project`, `volunteer`, `probation` |
| `industries` | `industry` (multi) | ID отраслей |
| `work_formats` | `work_format` (multi) | `REMOTE`, `HYBRID`, `ON_SITE` — регистр важен |
| `salary_from`, `salary_currency` | `salary`, `currency` | порог дохода + валюта |
| `only_with_salary` | `only_with_salary` | `true` — только вакансии с зарплатой |
| `labels` | `label` (multi, OR) | свободные строки; известные живые: `with_salary`, `with_address`, `not_from_agency`, `accept_handicapped`, `accept_kids`, `accredited_it`, `accept_labor_contract`, `night_shifts`, `low_performance`, `internship` |
| `part` | `part` (multi) | `temp`, `evening`, `weekend` — hh принимает, фильтрующего эффекта live (2026-09-25) не подтверждено |
| `order_by` | `order_by` | `relevance`, `publication_time_desc`, `salary_desc`, `salary_asc` |
| `search_period_days` | `search_period` | 1 / 3 / 7 / 30 дней |
| `exclude_words`, `exclude_in_description` | — (локальный каскад F2) | слова-исключения, название (+описание) |
| `company_blacklist`, `company_whitelist`, `whitelist_bypass` | — (локальный каскад) | ЧС/БС компаний |
| `exclude_agencies` | `label=not_from_agency` | без вакансий от кадровых агентств |
| `min_employer_rating`, `rating_period`, `no_reviews_policy`, `rating_cache_hours` | — (локальный каскад WF2-F4) | рейтинг работодателя |

Enum-значения `work_formats`, `part` и `order_by` валидируются на старте:
hh молча игнорирует неизвестные значения (проверено live 2026-09-25), поэтому
опечатка — это ошибка запуска, а не молча неполная выдача. `labels` —
свободные строки: hh добавляет новые метки без предупреждения. Имена из
прежних лет не работают: `no_agencies` hh игнорирует (живое имя —
`not_from_agency`), как и `accredited_it_employer` (живое — `accredited_it`);
`accept_temporary` эффекта не даёт. Всё, что не покрыто нативными фильтрами,
выражается текстом `query` (язык запросов выше).

Профили мультиотклика — всё из настроек автоотклика hh: поисковый запрос
(OR/AND/NOT/кавычки/скобки), `search_fields = ["name" | "company_name" |
"description"]` (пусто = все поля; живые счётчики для "python backend",
remote+RU, 2026-09-25: name=28, description=319, все поля=375),
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

### `[browser]`
Общая браузерная подсистема (T32): единая точка резолва Chrome для
headless-филлера тестов и браузерного редактора резюме. Цепочка:
`HHAUTO_CHROME_PATH` → `PATH` → кэш cloakbrowser (`~/.cloakbrowser`) →
автодонлоад.
- `auto_download` (дефолт true): скачать cloakbrowser-хромиум (свободная
  сборка с GitHub Releases, ~200 МБ; SHA-256 сверяется, когда источник
  публикует SHA256SUMS; вторичные URL — `CLOAKBROWSER_DOWNLOAD_URL`,
  версия — `CLOAKBROWSER_VERSION`).
- `humanize` (дефолт true): человекоподобный ввод в филлерах — плавный
  фокус, набор текста чанками с микроф-задержками, пейсинг действий.
- `preset`: `careful` (дефолт) — консервативный stealth-набор флагов
  запуска (`--disable-blink-features=AutomationControlled`, отключение
  `--enable-automation`) и медленный аккуратный пейсинг; `default` — без
  доп. флагов, обычный пейсинг.
- `channel`: `auto` (маркер автообновления или закреплённая свободная
  сборка), `free` (только закреплённая), `pro` (только кэшированные
  pro-сборки; лицензионная загрузка вне v1).

### TLS-fingerprint 1:1 (`hhauto tls-probe`)

Транспорт hh-запросов (tls-client) реплеит **реальный** ClientHello
cloakbrowser вместо ближайшего пресетного профиля:

```bash
hhauto tls-probe              # захват + кэш + replay-верификация
hhauto tls-probe --force      # переснять, даже если спека актуальна
hhauto tls-probe --host hh.ru # нестандартный хост захвата (по умолчанию — site из [api.endpoints])
```

- Механизм: браузер стартует с throwaway-профилем через локальный
  CONNECT-тап; первый TLS-рекорд туннеля (ClientHello, ~2 КБ) снимается
  байт-в-байт и релеится в реальный hh — SNI и проверка сертификата
  остаются на настоящем хосте.
- Кэш: `<data_dir>/browser/tls_spec.json`. Команда сама переснимает
  спеку при смене версии браузера или хоста; движок при старте грузит
  кэш и реплеит его на каждое hh-соединение.
- Что перегенерируется на каждое соединение (как у настоящего браузера):
  key shares, GREASE, ECH-grease, session id; порядок расширений
  переставляется per-connection. Что реплеится 1:1: наборы и порядок
  ciphers/extensions, группы, signature algorithms, ALPN, ALPS,
  compress-certificate.
- Без кэша транспорт деградирует на ближайший пресет с WARN в логе;
  устаревшая (по схеме) спека игнорируется, никогда не реплеится.

## Проверка

```bash
hhauto config validate   # fail-fast с указанием пути к ошибке
hhauto config template   # этот же шаблон из бинарника
```
