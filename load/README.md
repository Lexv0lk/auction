# load/

Нагрузочные сценарии шага 15 (см. `docs/load-testing.md` — полная инструкция, параметры и базовые результаты).

- `catalog_read.js` — чтение каталога с фильтрами и страницами; данные готовятся через административные формы перед прогоном.
- `bidding.js` — конкурирующие ставки участников на один лот через JSON API; повторы принятых ставок, расчёт 409 на устаревшую цену как ожидаемого исхода.
- `lib/` — общие модули: `config.js` (все параметры из окружения), `auth.js` (логин/сессии/CSRF), `k6utils.js` (UUID, точная целочисленная арифметика сумм), `metrics.js`, `report.js` (JSON-отчёт в `results/`).
- `verify/` — Go-команда послойной проверки БД после прогона (`go run ./load/verify`), юнит-тесты в `verify_test.go`.

Инструмент: **k6 v2.2.0** — внешний бинарник на хост-машине; в рантайм-образ приложения не входит (`.dockerignore`).

```bash
make load-catalog     # make load-catalog LOAD_VUS=15 LOAD_PROFILE=heavy
make load-bids        # make load-bids LOAD_VUS=12 LOAD_PAUSE=0.05 LOAD_PROFILE=heavy
make load-verify      # VERIFY_MODE=… VERIFY_… — команда печатается после прогона
```
