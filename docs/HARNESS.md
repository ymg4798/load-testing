# 개발 하네스

Claude Code로 이 프로젝트를 계속 개발하기 위한 구성입니다. 사람은 요청과 결정만 하고, 나머지는 정해진 순서로 돌아갑니다.

## 기본 사이클

```
/feature <요청>
  └ 브랜치 생성 → planner(설계·질문) → 공통 타입 커밋 → implementer ×N 동시 구현
    → verifier(/verify, /e2e) → reviewer → ui-designer(/ui-check) · perf-guard(/bench) → docs-writer
    → 작업 브랜치에 커밋 → 사용자에게 보고하고 master 병합 여부 질문
```

- master 병합, push, 태그, PR은 항상 사용자 확인 후에만 합니다.
- 애매한 선택(범위, 사용자에게 보이는 동작, 비용, 위험)은 planner가 질문으로 올리고, 메인 세션이 물어봅니다.

## 에이전트 (`.claude/agents/`)

| 에이전트 | 역할 | 모델 | effort | 편집 권한 |
|---|---|---|---|---|
| planner | 계획, 공통 타입, 작업 분할, 질문 | Opus | high | 없음 (읽기만) |
| implementer | 맡은 파일 안에서 구현 + 테스트 | Sonnet | high | 있음 |
| reviewer | 정확성·동시성·측정 정확도·자원 한계·보안 검토 | Opus | high | 없음 |
| verifier | 검증 실행, 실패 재현 보고 | Sonnet | medium | 없음 |
| ui-designer | 리포트·대시보드·관제 화면, 스크린샷 확인 | Opus | high | 있음 |
| perf-guard | 처리량·CPU·스케줄 지연 회귀 확인 | Sonnet | medium | 없음 |
| docs-writer | README(한국어)·DESIGN·예시 동기화 | Sonnet | low | 있음 |

- 모델 선택 기준: 판단이 결과를 좌우하는 일(설계, 리뷰, 화면)은 Opus, 범위가 정해진 양 많은 일은 Sonnet.
- planner와 reviewer는 `memory: project`로 이 코드베이스에서 배운 점을 `.claude/agent-memory/`에 쌓습니다.
- implementer는 끝날 때 바뀐 패키지 테스트(훅)를 통과해야 끝낼 수 있습니다.
- 에이전트는 다른 에이전트를 부르지 않습니다(조정은 메인 세션만).

## 스킬 (`.claude/skills/`)

| 명령 | 하는 일 | 시간 |
|---|---|---|
| `/verify [--quick]` | gofmt, vet, `-race` 전체 테스트, JS 문법, 예시 검증, 5개 플랫폼 빌드 | 약 2분 |
| `/e2e` | 테스트 서버(APM 에이전트) + 시나리오 실행, 결과 14가지 확인, 리포트 생성 | 약 30초 |
| `/ui-check <html>` | 라이트/다크 × 데스크톱/폰 스크린샷과 점검 목록 | 약 30초 |
| `/bench [--update]` | 초당 1만/3만 건 실측, 기준값과 비교 | 약 40초 |
| `/feature <요청>` | 위 사이클 전체 | 기능 크기에 따라 |
| `/release [버전]` | 검증, 변경 이력, 태그 제안 (push는 확인 후) | 수 분 |

`/feature`와 `/release`는 사람이 부를 때만 실행됩니다(`disable-model-invocation`).

## 훅 (`.claude/settings.json`)

- Go 파일을 고치면 바로 gofmt.
- 메인 세션이 끝내려 할 때(Stop), implementer가 끝내려 할 때(SubagentStop): HEAD 이후 바뀐 Go 패키지만 `go vet` + `go test`.
  실패하면 한 번 되돌아가서 고칩니다(`stop_hook_active`로 무한 반복 방지).

## 개인 설정

`.claude/settings.local.json`(git 제외)에 개인 권한이나 환경 변수를 두세요. 예: 자주 쓰는 명령 허용.

## 바꾸고 싶을 때

- 모델/effort: 각 에이전트 파일의 `model`, `effort`.
- 성능 기준값: `/bench --update` (같은 장비에서만 비교 의미 있음).
- e2e 확인 항목: `.claude/skills/e2e/scripts/e2e.sh`의 `checks` 목록.
