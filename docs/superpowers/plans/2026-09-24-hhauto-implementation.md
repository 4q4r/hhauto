# hhauto Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps tracked in `.sdd/progress.md` ledger; task reports in `.sdd/task<N>-report.md`.

**Goal:** Fully automated hh.ru job-search CLI (apply→chat→tests→interview) per `docs/PLAN.md`.

**Architecture:** modular monolith `internal/*`; contracts frozen in Task 0 (commit 28fe91e); network only in `hhapi/ai/proxy/webhook`; state in SQLite; feature modules register queue/scheduler tasks.

**Tech Stack:** Go 1.26+, cobra, pelletier/go-toml/v2, modernc.org/sqlite, x/net, x/time/rate, wneessen/go-mail, chromedp (opt-in), 99designs/keyring, x/crypto.

**Spec:** `docs/PLAN.md` (v1.1) — the binding authority for this plan.

## Global Constraints

- CGO_ENABLED=0, pure-Go deps only; no BurntSushi/toml, no robfig/cron.
- Contracts (internal/config/types.go + package interfaces) are immutable without orchestrator ruling.
- Quota defaults: 200 applies/day/account; autolift 6/day (4h timer + jitter); dry-run only via flag.
- Security invariants: TLS verify non-disableable, fail-closed proxy, redact logs always-on, kill-switch.
- Every task: TDD, gofmt, `go vet ./...`, `go build ./...`, `go test ./...` green before report.
- Conventional commits; branch per task; orchestrator merges and reconciles go.mod.

## Tasks

| ID | Owner (agent) | Files | Interfaces | Acceptance |
|----|---------------|-------|------------|------------|
| T0 | orchestrator | contracts bootstrap | see commit 28fe91e | vet+build clean ✅ |
| T1 | backend-architect | internal/config/{load,defaults,validate}.go, internal/logging/logging.go | consumes config types; produces config.Load/Validate, logging.New | TOML fixture tests, env-substitution, fail-fast validation, redaction test |
| T2 | backend-architect | internal/store/*, migrations/0001_init.sql | consumes config.Storage; produces store.Open + typed DAO groups | temp-db CRUD, migration idempotency, single-writer under -race |
| T3 | backend-architect | internal/proxy/impl, internal/queue/impl, internal/scheduler tests | proxy.Registry, queue.Queue, scheduler.Spec | socks5/http dialers (RFC1929), queue dedup/priority/drain, scheduler TZ/day/jitter tests |
| T4 | security-architect | internal/cookies/impl | cookies.Store | keyring+encrypted-file (Argon2id+XChaCha20), 0600 atomic writes, wrong-passphrase=ErrDecrypt |
| T5 | backend-architect | internal/webhook/impl | webhook.Notifier, config.Webhook | raw+HMAC+retry+outbox, telegram/discord presets, per-endpoint proxy |
| T6 | security-architect | internal/captcha/impl | captcha.Solver/Chain, config.CaptchaProvider | 2captcha+anticaptcha adapters, polling/timeouts, failover chain order, Balance |
| T7 | backend-architect | internal/hhapi/impl + testdata fixtures | hhapi.HHAPI | all methods per HH_API_MAP; typed errors incl. 302→captcha; retry policy (POST-no-key no auto-retry); httptest fixtures |
| T8 | ai-engineer | internal/ai/impl, internal/aiprompts/*.md (embed) | ai.Runner, ai.Provider | 3 dialects, 8 tasks w/ schemas, guard, cache, budget, ledger; golden fixtures; no-network unit tests |
| T9 | backend-architect | internal/profile/ | resume digest + profile.md loader (global + override) | auto-export parse, missing-fact flags |
| T10 | workflow-architect | features: autolift, multiapply, relevance, emplrating | features.Feature + hhapi/ai/store | WF1/WF2 trees incl. quota 200, rating filter (all/N days), no-links rule |
| T11 | workflow-architect | features: chatops, testsolve, interview, contacts, warmup, emaildup | features.Feature + ai tasks | WF3-WF9 incl. external forms draft_only, one-shot test submit (RECONCILE) |
| T12 | backend-architect | internal/engine, internal/review, internal/audit | engine.Supervisor drives features+queue | graceful shutdown, kill-switch, pause states, review queue SLA |
| T13 | backend-architect | internal/cli/* (cobra) | all packages | full command surface incl. onboarding, doctor, status, review, funnel |
| T14 | test-automation-engineer | internal/e2e tests | httptest hh fake end-to-end | apply pipeline dry-run path, chat classification path |
| T15 | devops-automator | Makefile, .goreleaser.yaml, CI | - | 6-target CGO_ENABLED=0 matrix green |
| T16 | technical-writer | README.md, docs/provenance.md, NOTICE | - | TOC/badges/Mermaid/disclaimer; provenance table with donor SHAs |

Waves: W1=T1..T6 (parallel), W2=T7,T8,T9 (parallel), W3=T10,T11,T12 (parallel), W4=T13, then W5=T14,T15,T16 (parallel). Reviews after each wave; ledger updated per task.
