# Task workflow transcrib

Проект на поддержке, поэтому здесь облегчённая модель: issue как исполнимая спека,
команда `возьми #NN`, milestones в трекере. Отдельные спеки и фазовые гейты
основного конвейера здесь не применяются.

Канон разработки: задача оформляется как GitHub Issue, владелец даёт команду `возьми #NN`, агент
доводит issue до PR в `dev`. Чат - оркестратор выполнения, GitHub Issue/PR - durable source of truth.

## Ветки и deploy

- `dev` - основная рабочая ветка. Merge в `dev` запускает CI/CD и deploy в Coolify dev.
- `feature/<slug>` - ветка одной issue, всегда от актуального `dev`.
- Параллельные issue выполняются в отдельных git worktree, по одному worktree на issue.
- `main` - prod-кандидат, обновляется только через release PR `dev -> main`.
- Release tag (`0.0.1`, `0.0.2`, `0.1.0`) - единственный ref для prod deploy.
- Prod deploy - только ручной запуск GitHub Actions `Deploy` на tag с `target=prod`.

Запрещено без явной команды владельца: merge PR, prod deploy, env/secrets, live DB mutation,
refund/payment action, destructive git/file operations.

## Основной цикл

1. Входящий баг/идея/замечание превращается в GitHub Issue.
2. Агент расследует факты: docs, код, логи, БД/runtime при необходимости.
3. Агент пишет issue как исполняемую спеку.
4. Для рискованных issue Architect, Tester и code reviewer проверяют спеку до разработки; blockers
   вносятся в issue.
5. В отдельном execution-чате владелец пишет: `возьми #NN`.
6. Агент выполняет issue до PR в `dev`, задает вопросы если видит проблемы.
7. Для рискованных issue проходит acceptance review: тесты, логика, архитектура, интеграция с кодовой
   базой и runtime, если он нужен.
8. Владелец ревьюит PR и отдельно даёт команду на merge.
9. После merge агент проверяет dev deploy, если задача требует runtime-проверки.

## Issue как спека

Команда владельца `возьми #NN` означает, что issue готова к выполнению. Лейбл `agent-ready` можно
использовать как backlog-маркер, но агент не требует его, если команда дана явно. Отдельный
документ-спека не заводится: issue самодостаточна.

Минимальная структура issue:

- Цель: какой результат нужен.
- Контекст: почему делаем, ссылки на каноны, инциденты, PR.
- Граница: что явно не трогаем.
- Алгоритм действий: императивные шаги для агента.
- Контракты: статусы, поля, функции, API, env, миграции, если меняются.
- Критерии приемки: проверяемые условия.
- Acceptance contract: инварианты и сценарии, которые защищают задачу. Если нужен автоматический
  acceptance test, issue фиксирует имя/место теста, входные данные, ожидаемый результат и что нельзя
  менять без согласования.
- Проверки: точные команды, smoke/E2E или причина, почему они не выполняются.
- Review gates: какие независимые проверки нужны перед merge: Tester, logic/domain, Architect,
  code review, DevOps/runtime. Для простой правки можно указать `не нужны`.
- Docs: какие каноны обновить или явно `docs не меняются`.
- Stop conditions: когда агент обязан остановиться.
- PR DoD: что должно быть в описании PR.

Если issue слишком большая, агент не реализует её одним diff. Он режет её на sub-issues с
самостоятельной приемкой и зависимостями. Команда `возьми #NN` на umbrella issue разрешает
декомпозицию, но код начинается только по отдельному sub-issue после команды владельца.
Трекер milestones при этом остаётся milestone-level.

Критерии большой issue: деньги, квоты, state machine, миграции, внешний API, provider runtime,
deploy/env, prod, много модулей или diff, который трудно быстро ревьюить.

Acceptance contract должен появляться до реализации. Тесты из него не являются "тестами агента",
который пишет код: это часть спеки. Если контракт неверный или конфликтует с реальным кодом/канонами,
implementation-agent останавливается и предлагает поправку issue, а не переписывает тест под код.

## Выполнение issue

Агент-оркестратор в execution-чате:

1. Проверяет `gh`/git доступ и актуальный `dev`.
2. Читает issue body, comments, связанные каноны.
3. Создаёт или открывает dedicated worktree и ветку `feature/<issue-slug>` от `dev`.
4. Делит работу на подзадачи и назначает исполнителей только если это нужно.
5. Если issue требует независимый acceptance test, он пишется до implementation diff отдельным
   агентом/коммитом или явно фиксируется как ручной smoke.
6. Реализует минимальный diff по issue, без соседних улучшений.
7. Не удаляет и не ослабляет acceptance tests без явного решения владельца в issue/PR.
8. Запускает проверки из issue и дополнительные проверки по риску (субагентное ревью).
9. Обновляет docs только если изменился факт, контракт, процесс, runtime или deploy.
10. Делает commit и push feature-ветки.
11. Открывает PR в `dev`.
12. Заполняет PR: summary, issue/spec compliance, verification, acceptance review, risks, not tested.

Агент не спрашивает владельца по ходу работы, если issue содержит достаточную спеку и нет stop
condition. Найденные соседние проблемы оформляются follow-up issue, а не чинятся в текущем diff.

## Параллельная работа

Можно вести несколько issue одновременно в разных чатах. Каждый execution-чат работает только в
своём worktree и своей ветке:

```text
../transcrib-issue-NN   -> feature/issue-NN-<slug>
../transcrib-issue-MM   -> feature/issue-MM-<slug>
```

Правила:

- перед правками агент проверяет `git status` и текущий worktree;
- один агент не правит файлы в worktree другого агента;
- общий repo checkout не используется для параллельной реализации;
- если нужный worktree уже занят другой issue, агент создаёт новый или останавливается;
- пересечения между задачами не резолвятся локально "заодно";
- расхождения веток решаются на этапе PR: rebase/merge от свежего `dev`, review и conflict resolution.

Почему так: изоляция worktree убирает случайное смешивание diff, а PR остаётся нормальным местом для
разрешения конфликтов между независимыми задачами.

## Роли агентов

- Orchestrator: ведёт чат, держит границы issue, принимает отчёты, делает git/GitHub действия.
- Worker: пишет узкий кодовый кусок по заданным файлам и контрактам.
- Tester: независимо фиксирует и проверяет acceptance tests/smoke, edge cases, race/idempotency,
  критические сценарии.
- Code reviewer: ищет баги, лишний код, слабые тесты, несоответствие issue.
- Architect: проверяет архитектуру, БД, деньги, state machine, provider/deploy контракты.
- DevOps: server, DB, Docker, deploy/runtime через `deploy/safe-ssh.sh`.
- Docs auditor: docs sync, если изменились каноны или runtime facts.

По умолчанию orchestrator делает всё сам. Subagents подключаются только по риску или когда это прямо
указано в issue.

Для денег, квот, state machine, миграций, provider runtime, deploy/env и prod-gates implementation и
acceptance должны быть разделены: код пишет Worker, приемку проверяет отдельный Tester/reviewer. Для
мелких правок достаточно самопроверки orchestrator'а, если issue прямо не требует review gates.

## Проверка

Перед PR агент обязан явно зафиксировать:

```text
Проверено:
- go test ...: pass
- go vet ./...: pass
- manual E2E: не запускался, причина ...
```

Для Go-изменений, которые затрагивают `internal/app`, state machine, БД, provider, billing,
deploy/config или runtime-поведение, локальный gate должен соответствовать CI:

```sh
cd src
NO_DOTENV=1 DATABASE_URL=postgres://transcrib:transcrib@localhost:5432/transcrib?sslmode=disable make ci-check
```

`go test ./...` без `DATABASE_URL` не считается полным gate: DB-backed тесты пропускаются.
`NO_DOTENV=1` нужен AI-агентам в sandbox, чтобы не читать локальные `.env` с секретами.
Deploy workflow обязан гонять тот же DB-backed gate до сборки image, иначе dev/prod rollout не
считается защищённым.

Для критических узлов нужны targeted tests: деньги, резерв квоты, идемпотентность, state machine,
гонки. Для Telegram UI, provider runtime, deploy и config допустим ручной E2E или dev smoke.

Acceptance tests, зафиксированные в issue, нельзя менять в implementation PR без явного согласования.
Если тест меняется, PR обязан объяснить, какой контракт изменился и где это согласовано.

Задача не закрыта, если критерии приемки и review gates из issue не проверены или непроверенные пункты
не названы явно.

## Контекст и длинные задачи

GitHub Issue и PR хранят состояние работы. Чат не является единственным источником правды.

Если задача длинная, перед compact или паузой агент пишет checkpoint в issue comment:

```text
Branch:
Done:
Changed files:
Verified:
Open risks:
Next step:
```

После compact или в новом чате агент продолжает из issue + последнего checkpoint.

Для execution-чата цель формулируется так: `довести issue #NN до PR в dev`. Goal mode помогает
вести сессию, но не заменяет issue.

## Закрытие PR

PR готов к owner review, если:

- scope совпадает с issue;
- CI/local checks зелёные или исключения явно объяснены;
- acceptance contract выполнен, а его тесты не ослаблены без согласования;
- review gates из issue пройдены: Tester, logic/domain, Architect, code review, DevOps/runtime или
  явно `не нужно`;
- docs синхронизированы, если менялся контракт;
- в PR перечислены риски и непроверенные пункты;
- нет blockers от reviewer/architect/tester.

Merge в `dev` выполняется только по явной команде владельца. После merge агент проверяет GitHub
Actions и Coolify dev, если задача требует runtime-проверки.

## Release

Release - отдельная задача:

1. Собрать release PR `dev -> main`.
2. Сверить вошедшие issue/PR.
3. Проверить, что в вошедших рискованных issue acceptance contract и review gates закрыты.
4. Пройти нужные smoke-тесты и `deploy/verify-deploy.sh`.
5. Зафиксировать release acceptance report: SHA, issues/PRs, проверки, runtime-smoke, риски, rollback.
6. Merge в `main` не выкатывает prod.
7. Prod deploy только по tag и явной команде владельца.
