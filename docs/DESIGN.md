# 설계

사용법은 [README](../README.md)에 있고, 이 문서는 구조와 그렇게 정한 이유를 다룹니다.

## 정한 것

| 항목 | 결정 | 이유 |
|---|---|---|
| 언어 | Go | 멀티코어로 초당 수만 건, 단일 실행 파일 배포 |
| 시나리오 | YAML 기본 + JS 함수 확장 (goja) | 단순한 건 YAML, 복잡한 흐름만 스크립트 |
| 프로토콜 | 플러그인 구조: http, ws, grpc, script, sleep, set | 엔진은 하나, 프로토콜은 부품 |
| 실행 | CLI + 터미널 실시간 출력 | CI에서도 그대로 사용 |
| 결과 | JSON(1초 스냅샷 포함) + HTML 리포트 + 비교 | 파일만으로 재현과 비교 가능 |
| 시각화 | HTML 리포트, 실시간 대시보드(SSE), Prometheus + Grafana | 모두 같은 1초 스냅샷에서 그림 |
| 분산 | 컨트롤러 1대 + 에이전트 N대, HTTP+JSON | 히스토그램을 합칠 수 있게 처음부터 설계 |
| 라이선스 | MIT | 공개용. k6(AGPL) 코드는 가져오지 않음 |
| 2차 기능 | Grafana Cloud k6의 기능 목록을 참고 (코드 아님) | 자동 분석, 트레이싱, 추이, CI 요약, 녹화 변환을 무료·로컬로 |

## 데이터 흐름

```
scenario.yaml ─▶ scenario.Load ─▶ engine.New (flow → protocol.Step 컴파일)
                                       │
                       runner (arrival-rate / users) ─▶ iterate ─▶ Step.Run ─▶ vu.Record
                                       │                                         │
                                       ▼                                         ▼
                               metrics.Aggregator ◀──────── 채널 (잠금 없음) ──────┘
                                       │ 매초 Snapshot (키별 HDR 히스토그램, 압축)
                                       ▼
                           run.Collect ─▶ Accumulator ─▶ Summary, Thresholds, Warnings
                                       │
                 Sink: 터미널 · 대시보드(SSE) · Prometheus      → Result(JSON) → HTML / compare

분산: 에이전트마다 위 흐름 → NDJSON으로 Snapshot 스트림 → 컨트롤러가 같은 초끼리 Merge → run.Collect
```

## 측정 원칙

1. **도착률 기반 실행이 기본.** 반복은 정해진 시각에 시작하며 이전 응답을 기다리지 않습니다.
   동시 진행이 `maxInFlight`에 닿으면 미루지 않고 `dropped`로 셉니다. 그래서 대상 서버는 보내는
   시점을 늦출 수 없고, coordinated omission이 구조적으로 생기지 않습니다.
   `examples/stall.yaml`로 서버를 1초 멈췄을 때 이 방식은 p99에 855ms가 잡혔고, closed model은 6.8ms로 가려졌습니다.
2. **스케줄 지연은 따로 기록합니다.** 예정 시각과 실제 시작의 차이는 생성기 쪽 문제(CPU 포화, OS 타이머)이므로
   지연시간에 섞지 않고 생성기 지표로 보여줍니다. wrk2처럼 섞고 싶으면 `options.correctLag: true`.
   - 처음 설계는 항상 섞는 것이었습니다. 그런데 macOS는 sleep을 꼬리 구간에서 수 ms씩 늦게 깨워서
     (측정: 1ms sleep의 p99가 0.85ms 초과, 최대 5ms 이상) 모든 요청이 부풀려졌습니다. 그래서 기본값을 바꿨습니다.
   - 스케줄러는 마지막 300µs를 바쁘게 대기(spin)해서 정밀도를 올립니다. 초당 수천 건 이상에서는 거의 계속 대기하게 되어 p99 지연이 0.3ms 이하입니다.
3. **통계는 HDR 히스토그램**(1µs~1h, 유효숫자 3자리)입니다. 1초마다 키(시나리오|단계)별 스냅샷을 압축해 남깁니다.
   스냅샷은 합칠 수 있으므로 분산 실행, 임계값, 대시보드, 비교가 모두 같은 데이터를 씁니다.
4. **생성기 자신을 감시합니다.** CPU, 스케줄 지연, 고루틴 수, OS 한도를 보고, 측정이 오염될 수 있으면 경고합니다.

## 2차 기능의 설계

| 기능 | 어떻게 | 왜 이렇게 |
|---|---|---|
| 자동 분석 (`run/insights.go`) | 끝난 뒤 1초 스냅샷만 읽는 규칙 몇 개 | 저장된 결과로 재현 가능, 분산 실행에서도 그대로 |
| 한계 조건 (`run/limits.go`) | 매초 최근 N초 스냅샷으로 임계값을 판정하는 Sink, 실패 시 실행 컨텍스트 취소 | 임계값 문법·코드 재사용. 로컬과 분산이 같은 경로 |
| 트레이싱 (`protocol/trace.go`) | 요청마다 VU의 난수로 trace/span ID 생성, 헤더 주입 | 잠금 없음. 에이전트끼리 겹칠 확률 무시 가능(128비트) |
| 예시 요청 (exemplar) | 키마다 초당 가장 느린 1건 + 실패 최대 10건만 스냅샷에 | 스냅샷 크기 거의 그대로, 합치기 쉬움 |
| 서버 지표 (`monitor/`) | Prometheus 텍스트 파서 직접 구현, 1초마다 수집, Decorator로 스냅샷에 붙임 | 의존성 없음. 컨트롤러에서만 수집해 분산에서도 중복 없음 |
| 기록 (`history/`) | 테스트별 폴더, gzip 결과 + `index.jsonl` | 목록·추이는 index만 읽음. DB 불필요 |
| CI·알림 (`output/markdown.go`, `notify/`) | 마크다운 요약, 웹훅, Grafana annotation API | 외부 서비스 없이 무료로 |
| 감시 모드 (`cmd/lt/monitor.go`) | 시나리오를 `iterations` 실행기로 바꿔 주기 실행 | 부하테스트 시나리오를 그대로 재사용 |
| 변환기 (`convert/`) | HAR·curl·OpenAPI → YAML, 생성 후 검증까지 | 손으로 쓰는 시간을 줄임 |

## 3차: 와탭식 관제 기능

| 기능 | 어떻게 |
|---|---|
| 히트맵 (`metrics/hits.go`) | 초마다 응답시간을 로그 눈금 60칸(100µs~100s, 10칸/10배)에 성공·실패로 나눠 셈 + 저장 샘플 최대 60개(reservoir). 스냅샷에 붙어 분산 실행에서도 합쳐짐 |
| 액티브 트랜잭션 | 엔진의 slot마다 시작 시각·현재 단계를 atomic으로 기록, 대시보드가 `/api/active`로 조회. 서버 쪽은 에이전트가 1초마다 보고(5초 TTL) |
| 알림 (`run/alerts.go`) | 매초 최근 window의 스냅샷으로 조건 판정, `for` 동안 유지되면 firing, 풀리면 resolved. 부하 지표(임계값 문법)와 `server{이름}` |
| DB 감시 (`monitor/`) | pgx, go-sql-driver/mysql, Redis는 RESP 직접. Top SQL은 pg_stat_statements / performance_schema / commandstats의 시작·끝 차이 |
| APM 수집기 (`apm/`) | OTLP/HTTP(protobuf는 protowire로 직접 해석, JSON) + 에이전트 액티브 보고. 트레이스 10분/2만 개/20만 span 상한, 작업별 HDR(유효숫자 2) |
| 에이전트 (`agent/`) | 표준 라이브러리만: HTTP 미들웨어(traceparent·b3·uber 이어받기), database/sql 드라이버 래퍼, RoundTripper. OTLP JSON으로 1초마다 전송, 요청 처리를 막지 않음 |
| lt watch (`cmd/lt/watch.go`) | 부하 없이 1초 스냅샷을 만들어 monitor·APM이 채우고, 알림·대시보드·Prometheus로 보냄. 최근 `--keep`만큼만 기억 |

## 4차: 범위 확장

| 기능 | 어떻게 |
|---|---|
| 프로토콜 (`protocol/{sqlstep,redisstep,graphqlstep,tcpstep}`) | SQL은 `{{ }}`를 바인드 파라미터로 컴파일, 시나리오당 `*sql.DB` 공유. Redis는 VU당 연결(RESP2). GraphQL은 HTTP 연결 풀 공유 |
| 녹화 (`record/`) | `httputil.ReverseProxy` 기반 정방향/역방향 프록시, `--mitm`은 로컬 ECDSA CA로 호스트별 인증서. 끝나면 HAR 변환 경로 재사용 |
| 컨테이너 (`monitor/docker.go`) | Docker Engine API 직접 호출, one-shot stats를 초마다, CPU는 직전 표본과의 차이 |
| 로그 (`logs/`) | 링 버퍼(20만 줄/64MB) + 초당 레벨별 카운터, 파일은 폴링 tail -F, OTLP 로그는 APM 포트에 Mount. 에러 메시지 정규화로 묶음 |
| 액티브 스택 (`agent/stack.go`) | 요청 goroutine에 pprof 라벨, 1초 넘은 요청이 있을 때만 goroutine 프로파일(debug=1)에서 라벨로 스택을 찾음 |
| 서비스 맵 (`apm/topology.go`) | span의 부모-자식과 kind로 client/service/db/http 노드와 간선(호출 수, 에러, 평균, p95) |
| 감시 기록 (`store/`) | 시간별 세그먼트 파일(블록마다 CRC + DEFLATE) + 분 단위 사전 집계 파일. 긴 범위는 분 집계만 읽음 |

## 패키지

```
cmd/lt/                 CLI (run, validate, report, compare, agent, preflight)
cmd/testserver/         검증용 대상 서버 (지연·지터·에러·멈춤 주입, WS, gRPC health)
internal/
  scenario/             YAML 로드, 검증·기본값, 데이터 소스, 에이전트 분할(Partition)
  engine/               스케줄(구간별 선형), arrival-rate / users 실행기, 반복 실행
  protocol/             Step·VU·Env 인터페이스, 등록소, check/extract, 에러 분류
    httpstep/ wsstep/ grpcstep/ builtin/
  script/               goja: script 단계, setup
  tmpl/                 {{ }} 템플릿
  jsonpath/             JSONPath 부분집합
  metrics/              HDR, Aggregator, Snapshot/Merge, Accumulator/Summary, 임계값
  run/                  Collect(싱크 분배, 결과 작성), Local 실행, 경고, 자동 분석, 한계 조건, 결과 파일
  output/               터미널
  web/                  HTML 리포트 + 실시간 대시보드 (같은 페이지, uPlot 내장)
  prom/                 /metrics
  compare/              결과 비교
  cluster/              agent / controller
  preflight/            OS 한도 점검
  monitor/              서버 지표 수집 (Prometheus 텍스트, 프로세스)
  history/              실행 기록, 기준 결과
  notify/               웹훅, Grafana annotation
  convert/              HAR / curl / OpenAPI → 시나리오
  apm/                  트레이스 수집기 (OTLP/HTTP, 에이전트 액티브 보고, 서비스 맵, 에러 묶음)
  logs/                 로그 저장·검색 (파일 tail, OTLP 로그)
  store/                감시 기록 디스크 저장
  record/               녹화 프록시
agent/                  Go 앱용 APM 에이전트 (공개 패키지)
deploy/                 Prometheus + Grafana (docker compose, 대시보드 JSON)
examples/               시나리오 예시
```

### 프로토콜 플러그인

```go
type Factory func(raw any, env *protocol.Env) (protocol.Step, error)
protocol.Register("http", compile)

type Step interface {
    Run(ctx context.Context, vu *protocol.VU) error   // 측정은 vu.Record(Result{...})
}
```

- `Env.Shared`: 시나리오 안에서 VU들이 공유하는 자원(HTTP Transport, gRPC 연결 풀, 컴파일된 JS)
- `VU.State`: VU 하나에 속한 자원(쿠키 저장소, JS 런타임, WS 연결)
- 새 프로토콜은 패키지 하나를 만들고 `engine`에서 blank import 하면 됩니다.

## 분산 실행 프로토콜

| 요청 | 내용 |
|---|---|
| `POST /v1/run` | 시나리오 폴더(파일 묶음), 에이전트 번호/총수, 시작 지연. 에이전트가 컴파일까지 하고 오류를 즉시 반환 |
| `GET /v1/stream?id=` | NDJSON. 1초 스냅샷, 마지막에 `{"done":true}` |
| `POST /v1/stop` | 중단 |
| `GET /v1/status` | 사용 중 여부 |

- **시작 동기화**: 절대 시각이 아니라 "요청 받고 N초 뒤"로 맞춥니다. 시계가 어긋나도 네트워크 지연 정도의 오차만 생깁니다.
- **합치기**: 같은 초(T)의 스냅샷이 모든 에이전트에서 오면 합칩니다. 한 에이전트가 3초 이상 늦으면 기다리지 않습니다.
- **나누기**: 도착률은 1/N, 사용자 수는 몫 + 나머지를 앞 에이전트부터 배분, 데이터 행은 `i % N`으로 나눕니다.

## 검증 결과 (Apple M 계열 노트북, 대상 서버도 같은 장비)

| 항목 | 결과 |
|---|---|
| 초당 1만 / 3만 / 5만 건 | 정확히 달성, 생성기 CPU 24% / 29% / 43%, 스케줄 지연 p99 33µs / 88µs / 264µs |
| 단계별 부하 반복 수 | 계획 1875회 = 실행 1875회 |
| 분산(에이전트 2대) | 합계가 단일 실행과 일치 (1876 vs 1875, 에이전트 간 반올림 차이) |
| 서버 1초 멈춤 | open model p99 855ms, closed model p99 6.8ms (멈춤이 가려짐) |
| 테스트 | `go test -race ./...` 통과 (단위 + HTTP/WS/스크립트/분산 통합) |

## 남은 일 / 한계

- gRPC 스트리밍 호출 미지원 (단항 호출만)
- JS 스크립트 안에서 측정되는 HTTP 호출 미지원 (setup에서만 `http` 사용 가능)
- 에이전트 통신은 평문 HTTP + 토큰. 공개망에서는 SSH 터널이나 VPN 사용
- Windows에서는 생성기 CPU 측정이 0으로 나옴
- HTTP/3, 요청 단계별 세부 시간(DNS·연결·TLS·첫 바이트) 미지원
- 트레이싱은 WebSocket과 JS 스크립트 단계에는 붙지 않음 (HTTP, gRPC만)
- 자동 분석은 규칙 기반이라 원인을 단정하지 않음. "언제, 어떤 부하에서, 무엇이 함께 변했는지"까지만
- 브라우저 테스트(Web Vitals)와 클라우드 부하 지역은 범위 밖
- APM 에이전트는 Go만 직접 제공. 다른 언어는 OpenTelemetry로 연결 (실제 Java/Python SDK와의 연동은 아직 미검증, OTLP 명세 기준으로만 시험)
- 서버 트랜잭션 히트맵은 에이전트가 1초마다 보내므로 1~2초 늦게 반영됨
- 감시 모드(watch)는 기록을 메모리에만 둠 (재시작하면 사라짐)
