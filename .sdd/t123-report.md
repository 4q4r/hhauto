# T123 — Rejection Loop (MVP: отчёт + review-парковка решений владельца; авто-правок НЕТ)

Ветка: `feat/t123-rejection-loop` @ `/tmp/hhauto-wt112`, коммит **5247cd2** (ДО отчёта, по мандату). База: origin/main 397f2dc. НЕ смержено, НЕ запушено — решение за контролёром.

## 1. Контракт (итог)

### Пайплайн
```
hhauto rejections analyze [--days N] [--account ID]
  → collectDiscard: SSR /applicant/negotiations?filter=discard&page=1..20
      (R9-паттерн: HH-Lux-InitialState → applicantNegotiations.topicList)
      пост-фильтр IsStructuralRefusal(lastState) + окно N дней (дефолт 30)
      стоп: пустая страница / вся страница старше окна / кап страниц
  → на каждый свежий отказ:
      skip, если (account, vacancy) уже в rejections (идемпотентность, 0 токенов)
      VacancyCard GET (сбой/архив → карточка из item, desc_missing)
      ChatHistory GET по neg.ChatID (живой контракт: item.chatId)
      ai.Runner(reject_analysis): вход = карточка (desc HTML→текст, кап 12k) +
      транскрипт (кап 40 сообщений × 800 рун) как НЕдоверенные данные +
      resume_digest/profile_text как доверенные факты
  → вердикт needs_human=true → НЕ хранится (урок донора: шаблонные причины
    были бы догадками), счёт в отчёте
  → evidence_based = (число входящих сообщений работодателя > 0) — считает Go,
    не модель; догадки ХРАНЯТСЯ с флагом 0 («догадки помечаются, не выбрасываются»)
  → отчёт: консоль (категории с долями ТОЛЬКО от подтверждённых; догадки
    отдельным списком без долей; fix-plan) + md-файл {data_dir}/rejections-report.md
```

### ИИ-задача reject_analysis (12-я, паттерн injectSetkaTasks-семейства)
- `internal/ai/tasks.go` + `contracts.go`: `TaskRejectionAnalysis TaskName = "reject_analysis"`.
- Схема `internal/ai/schemas/reject_analysis.json` — строгая (additionalProperties=false, все ключи required): `{confidence, needs_human, needs_human_reason, injection_suspected, root_cause, critique, improved_letter(string|null), missing_skills[≤30], about_me_rec, takeaway}` + общие поля.
- Промпт `internal/aiprompts/reject_analysis.md` (builtin, v1): донорские правила §4.4 t110 — запрет приписывать опыт/числа/сертификаты/стаж; improved_letter без чисел вне доверенных фактов, без зарплаты/CEFR/стажа; missing_skills только из описания вакансии (нет описания → []); root_cause без слов работодателя обязан начинаться с «Предположительно:» и confidence ≤ 0.6.
- Анти-фабрикация (T91/T119-класс) в `runner.go postChecks`: `TaskRejectionAnalysis` идёт через `checkFreeText` (forbiddenTopic + numberLeak по improved_letter; транскрипт НЕ входит в trusted — придуманные работодателем числа в письмо просочиться не могут), numberLeak-условие расширено `cover_letter || reject_analysis`.

### Схема хранения (миграция 0007, expand-only)
```sql
CREATE TABLE IF NOT EXISTS rejections (
    id INTEGER PRIMARY KEY,
    account_id TEXT NOT NULL, vacancy_id INTEGER NOT NULL,
    vacancy_name TEXT NOT NULL DEFAULT '', employer_name TEXT NOT NULL DEFAULT '',
    negotiation_id TEXT NOT NULL DEFAULT '',
    rejected_at TEXT NOT NULL,             -- дата отказа (live: lastModified)
    root_cause TEXT NOT NULL,
    evidence_based INTEGER NOT NULL DEFAULT 0,
    improved_letter TEXT NOT NULL DEFAULT '',
    missing_skills TEXT NOT NULL DEFAULT '[]',
    takeaway TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL               -- db.now(), не time.Now (урок T72)
);
CREATE UNIQUE INDEX idx_rejections_key ON rejections (account_id, vacancy_id);  -- идемпотентный upsert
CREATE INDEX idx_rejections_account ON rejections (account_id, rejected_at);
```
DAO `internal/store/dao_rejections.go`: `RejectionsPut` (upsert), `RejectionsGet` (nil при отсутствии — гейт повтора), `RejectionsList` (новые сверху, источник отчёта).

### ЖИВЫЕ HH-КОНТРАКТЫ (зонд live123, дампы /tmp/opencode/t123/*.html)
1. **Параметр `filter=discard` hh игнорирует на SSR** (`filterInUse:"active"`, total=586 — общий список вперемешку). Рабочий фильтр — клиентский: `lastState` item'а. Донор T110 знал это же («клик по вкладке» — клиентская перерисовка). Метод `NegotiationsDiscard` параметр передаёт (контракт донора), фича пост-фильтрует `IsStructuralRefusal` — гвард от ЛЮБОЙ поверхности.
2. **Item-форма живого topicList** отличается от старой фикстуры: `id`/`chatId` — ЧИСЛА (парсер переведён на scalarString), `vacancyName`/`employerName`/`updatedAt` ОТСУТСТВУЮТ; дата = `lastModified` (fallback'и сохранены), имя/работодатель добираются из карточки вакансии; прямой **`chatId`** в item убирает sweep по chatik/api/chats — evidence берётся одним ChatHistory на отказ.
3. Сортировка страниц — по свежести (стоп-условие «страница старше окна» живо подтверждено).

### Команда
`hhauto rejections analyze [--days N] [--account ID]` — без `[ai.tasks.reject_analysis]` → fail-fast «добавьте секцию [ai.tasks.reject_analysis] с model_profile». Конфиг владельца НЕ тронут: live-прогон шёл через копию конфига в /tmp/opencode/t123/hhauto.toml (оригинал байт-в-байт, проверено diff) + `[ai.tasks.reject_analysis] model_profile="letter"`.

### Отчёт-пример (консоль, живой прогон 2026-10-10/11, EXIT=0)
```
✓ Разбор отказов: в окне 30 дн. найдено 153 · разобрано 116 · уже в базе 7 · отклонено ИИ 30 · догадок 108
Причины отказов (подтверждены работодателем):
  · локация/формат — 2 (25%)
  · навыки/стек — 4 (50%)
  · грейд/стаж — 1 (12%)
  · качество отклика — 1 (12%)
Догадки (нет слов работодателя — оценка, не факт):
  · выбрали другого — 11
  · зарплата — 7
  · другое — 4
Fix-plan (решение за владельцем — авто-правок нет):
  · навыки (top): Kubernetes (23), Grafana (13), Redis (13), Ansible (12), Kafka (12), ClickHouse (11), Prometheus (9), TypeScript (8), gRPC (8), Node.js (7)
Отчёт: /home/whoami/.local/share/hhauto/rejections-report.md
```
Доли считаются только от подтверждённых (8 строк со словами работодателя): 4/8=50%, 2/8=25%, 1/8=12%. 116+7+30 = 153 (по строке на вакансию: две переговорки по одной вакансии схлопнулись upsert-ом — 122 строк в БД).

## 2. Файлы (23)
- store: `migrations/0007_rejections.sql`, `dao_rejections.go`, `dao_rejections_test.go`, `types.go` (+Rejection), `migrate.go` (+v7), `store_test.go` (пины версии 6→7 — ко-правка миграционного счётчика)
- ai: `contracts.go`, `tasks.go`, `runner.go` (postChecks +2 ветки), `schemas/reject_analysis.json`, `schemas/schemas_test.go` (+allNames), `aiprompts/reject_analysis.md`, `reject_analysis_test.go`
- hhapi: `contracts.go` (+NegotiationsDiscard, +ChatID), `client.go` (negotiationsPage-хелпер — дедуп 40 строк), `client_test.go`, `testdata/negotiations_discard.html` (живая форма), `live_t123_discard_probe_test.go` (зонд, тег live123, вне гейта — прецедент live87)
- feature: `internal/features/rejections/rejections.go` + `rejections_test.go` (пакет №39)
- cli: `cmd_rejections.go`, `cmd_rejections_test.go`, `cli.go` (+AddCommand)

## 3. Тесты (TDD RED→GREEN на каждом слое; RED фиксирован vet-падением «undefined» до имплементации)
- store `TestRejectionsRegistry`: upsert-идемпотентность (re-analysis в той же строке), frozen db.Now, list new-first, изоляция аккаунтов.
- ai: `TestRunRejectAnalysisNumberNotInSources` (выдуманное число в improved_letter → needs_human + number_not_in_sources), `...GroundedLetterPasses` (число из профиля проходит), `TestRejectAnalysisBuiltinPrompt`; schemas_test allNames (строгость+компиляция).
- hhapi `TestNegotiationsDiscardFromSSR`: filter=discard на проводе, chatId-число, lastModified, смешанный список (RESPONSE рядом с DISCARD парсится — фильтр клиента).
- feature (фейк-API + скриптованный Runner): happy path (RESPONSE-элемент скипается, имена из карточки, evidence/guess флаги), фейковый вердикт (needs_human) НЕ хранится, идемпотентность (2-й прогон: 0 вызовов ИИ, SkippedExisting), empty-case, окно исключает старые, сбой карточки не валит анализ.
- cli `TestRejectionsAnalyzeCmdFailsFastWithoutAI` (runRoot через cobra-дерево).

## 4. Гейт (после живых правок контракта, финальный)
gofmt -l: 0 файлов · go vet ./...: 0 · go build: OK · CGO_ENABLED=0: OK (статический ELF) · golangci-lint run: **0 issues** · go test -race -p 2 -count=1 ./...: **39/39 пакетов ok, exit 0** (38 базовых + новый пакет rejections; cli ~130с — учтено).

## 5. Live-прогон (только GET; никаких POST/мутаций; ENV не ставились)
- Зонд live123: 3 дампа (discard-p1/p2, all-p1) → контракты §1; дампы сохранены /tmp/opencode/t123/.
- Полный прогон `rejections analyze --days 30` (бинарь worktree, копия конфига, EXIT=0): **найдено 153, разобрано 116, уже в базе 7 (идемпотентность), отклонено ИИ-гвардами 30** (injection_suspected — инструкции в переписке; number_not_in_sources — выдуманные числа в improved_letter), **догадок 108**; **8 отказов со словами работодателя** → доли категорий: навыки/стек 4 (50%), локация/формат 2 (25%), грейд/стаж 1, качество отклика 1.
- Fix-plan живьём: Kubernetes (23), Grafana (13), Redis (13), Ansible (12), Kafka (12), ClickHouse (11), Prometheus (9), TypeScript (8), gRPC (8), Node.js (7) — согласуется с профилем владельца (DevOps-семья резюме).
- Пример живого разбора (vacancy 137977431 Python/SaaSoft): root_cause «Предположительно: не пройдены формальные фильтры — требуется от 4 лет…» + improved_letter, заземлённый реальными проектами профиля (anicli-py, cindex, FastAPI/Django/Docker), + missing_skills[12] + takeaway про расхождение «в письме 4 года / в резюме ~2г9м».
- Миграция 0007 применилась к живой БД владельца штатно (expand-only); таблица rejections наполнена 122 живыми строками — фича сразу полезна владельцу. MD-отчёт: ~/.local/share/hhauto/rejections-report.md; полный лог: /tmp/opencode/t123/live-run2.log.
- Нюанс (не дефект): у части evidence_based-строк модель всё равно пишет «Предположительно:» — слова работодателя в чате есть, но явной причины не называют; флаг отражает наличие данных, текст — уверенность. Донорская семантика «догадки отдельно и без долей» соблюдена.

## 6. Решения MVP-среза (по мандату) и follow-ups
- БЕЗ авто-правок «О себе», БЕЗ adaptive_skills, БЕЗ review-парковки (парковка не нужна: отчёт-only, решать владельцу нечего применять автоматически). Fix-plan — текстом в отчёте.
- Follow-up (не в этом PR): (а) вебхук-событие на новый разбор отказа; (б) включение категорий-долей в ответ скорера (уроки отказов как контекст) — только после накопления подтверждённых причин; (в) дедуп «Повторный отклик бессмыслен» в candidate-выбор; (г) daemon-задача периодического анализа (сейчас — ручная команда).

## 7. Отклонения от буквы мандата (зафиксировано)
- «список отклонённых (вакансия, дата)»: дата отказа = lastModified (живой факт; updatedAt на поверхности не существует).
- «карточка из кэша»: кэша карточек нет — карточка тянется GET-ом на каждый свежий отказ (кэш вердиктов ИИ внутри Runner остаётся); при сбое — данные из item.
- «chat-истории как evidence» — через прямой chatId item'а (живой контракт), а не sweep списка чатов: меньше GET-ов, тот же evidence.
