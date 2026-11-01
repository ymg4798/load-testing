# lt

개발용 부하테스트 도구. YAML로 시나리오를 쓰고, 필요한 부분만 JavaScript로 처리합니다.
HTTP · WebSocket · gRPC를 지원하고, 여러 대로 나눠 돌릴 수 있습니다.

- **정확한 측정**: 정해진 시각에 요청을 보내는 open model 방식. 서버가 느려져도 보내는 양이 줄지 않습니다.
- **HDR 히스토그램** 기반 p50~p99.9. 1초 단위 기록을 남겨 분산 실행 결과도 정확히 합칩니다.
- **CI 판정**: 임계값을 못 지키면 exit 1.
- **자동 분석**: "41초, 초당 1,013회부터 지연 급증", "그때 서버 CPU 103% → 211%"처럼 문제 지점을 문장으로 알려줍니다.
- **한계 찾기**: 부하를 올리다 조건(`p95 < 200ms` 등)이 깨지면 멈추고, 버틴 최대 부하를 알려줍니다.
- **서버 지표 같이 보기**: 대상 서버의 Prometheus 지표나 프로세스 CPU·메모리를 1초 단위로 함께 기록합니다.
- **분산 트레이싱 연동**: 요청마다 trace ID를 붙이고, 가장 느린/실패한 요청의 ID를 리포트에 남깁니다.
- **기록과 추이**: 실행 결과를 쌓아두고 기준 결과와 비교하거나 추이 그래프로 봅니다.
- **CI·알림**: GitHub Actions 요약, Slack/Discord/웹훅 알림, Grafana 구간 표시.
- **변환기**: 브라우저 HAR, curl 명령, OpenAPI 문서를 시나리오로 바꿉니다.
- **감시 모드**: 같은 시나리오를 주기적으로 돌려 서비스가 살아 있는지 확인합니다.
- **시각화**: 터미널, HTML 리포트, 실시간 대시보드, Prometheus/Grafana.
- **단일 실행 파일**: 설치할 런타임이 없습니다. 위 기능 모두 외부 유료 서비스 없이 동작합니다.

```
  time  target/s    iter/s     req/s       p50       p95       p99    err%   drop inflight   cpu
    1s       200       199       199   13.77ms   21.36ms   28.11ms   0.00%      0        2    1%
    2s       200       200       200   12.81ms   21.20ms   29.10ms   0.00%      0        2    1%
```

## 설치

```bash
make build            # bin/lt, bin/testserver
# 또는
go install github.com/ymg4798/lt/cmd/lt@latest
```

## 5분 사용법

```bash
bin/testserver &                                  # 연습용 대상 서버 (:8080, gRPC :50051)
bin/lt run examples/basic.yaml                    # 실행
bin/lt run examples/login-flow.yaml -o r.json --html r.html   # 결과 파일 + HTML 리포트
bin/lt run examples/basic.yaml --dashboard :5665  # 실시간 대시보드
bin/lt compare old.json new.json                  # 이전 결과와 비교 (회귀 시 exit 1)
```

| 명령 | 하는 일 |
|---|---|
| `lt run [flags] <scenario.yaml>` | 테스트 실행 |
| `lt validate <scenario.yaml>` | 실행 없이 시나리오 검사 |
| `lt report <result.json> [-o out.html]` | 결과 파일을 HTML 리포트로 |
| `lt compare <base.json> <new.json>` | 두 결과 비교 (`--max-latency 10`, `--max-errors 1`, `--noise 1ms`) |
| `lt history [test] [--html trend.html]` | 저장된 실행 목록, 추이 그래프 |
| `lt history pin <result file>` | 기준 결과로 지정 (`--baseline pinned`) |
| `lt convert <file>` | HAR / curl / OpenAPI → 시나리오 YAML |
| `lt record [--target URL \| --listen :8888]` | 프록시로 실제 트래픽을 녹화해 시나리오로 |
| `lt monitor [flags] <scenario.yaml>` | 주기적 점검 + 상태가 바뀌면 알림 |
| `lt watch [flags] <watch.yaml>` | 상시 관제 화면: 서버·DB 지표, APM, 알림 |
| `lt agent [--listen :7700] [--token T]` | 분산 실행용 에이전트 |
| `lt preflight` | OS 한도 점검 (열린 파일 수, 포트 범위 등) |

`lt run` 플래그:

| 플래그 | 하는 일 |
|---|---|
| `-o result.json`, `--html report.html`, `--md summary.md` | 결과 파일, HTML 리포트, 마크다운 요약 |
| `--dashboard :5665`, `--prometheus :9464` | 실시간 대시보드, Prometheus 지표 |
| `--agents h1:7700,h2:7700`, `--token` | 분산 실행 |
| `--save`, `--history-dir` | 결과를 기록 폴더에 저장 (기본 `.lt/runs`, `$LT_HISTORY`) |
| `--baseline last\|pinned\|파일` | 기준 결과와 비교, 회귀 시 exit 1 (`--max-latency`, `--max-errors`, `--noise`) |
| `--notify URL`, `--notify-on always\|fail` | 끝나면 웹훅 알림 (`$LT_NOTIFY`) |
| `--apm :4318` | 서버의 트레이스 수신 (lt 에이전트, OpenTelemetry) |
| `--grafana http://localhost:3000` | Grafana 대시보드에 테스트 구간 표시 (`$LT_GRAFANA_URL`, `$LT_GRAFANA_TOKEN`) |
| `-e name=value`, `--label`, `-q`, `--keep` | 변수 덮어쓰기, 메모, 초당 출력 끄기, 대시보드 유지 |

종료 코드: `0` 통과 · `1` 임계값 실패, 회귀, 한계 조건으로 중단 · `2` 시나리오 오류 · `3` 실행 실패.

## 시나리오 파일

```yaml
name: login-and-browse

vars:                         # {{ baseUrl }} 로 사용. -e baseUrl=... 로 덮어쓰기
  baseUrl: http://localhost:8080

data:                         # 반복마다 한 행씩 {{ users.id }}
  users: { csv: users.csv, mode: sequential }   # sequential | random | unique (json:, rows: 도 가능)

setup: hooks.js#setup         # 시작 전 1회 실행, 반환 객체가 전역 변수가 됨

options:
  timeout: 5s                 # 요청 타임아웃 (기본 30s)
  http2: true                 # TLS에서 HTTP/2 (기본 true)
  insecure: false             # TLS 인증서 검증 생략
  cookies: false              # VU별 쿠키 저장
  noKeepAlive: false
  maxConnsPerHost: 0          # 0 = 무제한
  gracefulStop: 30s           # 끝날 때 진행 중인 반복을 기다리는 시간
  correctLag: false           # true면 스케줄 지연을 첫 요청 지연시간에 더함 (wrk2 방식)
  tracing: true               # 요청마다 trace 헤더 (아래 "분산 트레이싱" 참고)
  limitWindow: 5s             # limits를 판정할 최근 구간

scenarios:                    # 여러 개를 동시에 실행
  browse:
    executor: arrival-rate    # 초당 N회 시작 (기본)
    stages:                   # 또는 rate: 100 + duration: 1m
      - { duration: 30s, target: 100 }
      - { duration: 2m,  target: 1000 }
      - { duration: 30s, target: 0 }
    maxInFlight: 2000         # 동시 진행 상한. 넘으면 지연 대신 dropped로 집계
    startAfter: 10s
    onError: abort            # abort(기본): 실패하면 그 반복의 나머지 단계 생략 | continue
    flow:
      - http: { ... }
      - script: hooks.js#pickItem
      - sleep: 500ms..2s
  steady:
    executor: constant-users  # 사용자 N명이 쉬지 않고 반복 (closed model)
    users: 20
    duration: 3m
    flow: [ ... ]
  # executor: ramping-users + stages(target = 사용자 수)
  smoke:
    executor: iterations      # 정해진 횟수만 실행 (users명이 나눠서). duration은 최대 대기 시간(기본 10m)
    iterations: 10
    users: 2
    flow: [ ... ]

thresholds:
  latency: ["p95 < 300ms", "p99 < 1s"]           # 모든 요청
  "latency{step:item}": ["p99 < 500ms"]           # 특정 단계
  "latency{scenario:browse}": ["avg < 100ms"]
  "iteration{scenario:browse}": ["p95 < 2s"]      # 반복 전체 시간
  errors: ["rate < 1%"]                           # rate | count
  dropped: ["count == 0"]
  rps: [">= 900"]

limits:                       # 문법은 thresholds와 같음. 최근 limitWindow 동안 하나라도 깨지면 즉시 멈춤
  latency: ["p95 < 500ms"]
  errors: ["rate < 5%"]

monitor:                      # 테스트하는 동안 서버 지표를 1초마다 같이 기록
  - url: "{{ baseUrl }}/metrics"            # Prometheus 형식 엔드포인트
    metrics: [process_cpu_seconds_total, go_goroutines, 'http_requests_total{code=~"5.."}']
  - process: postgres                       # 같은 장비의 프로세스 (이름 또는 pid): CPU %, 메모리
```

통계 이름: `min`, `avg`, `med`, `max`, `p90`, `p95`, `p99`, `p99.9`, `pNN`. 단위 없는 숫자는 ms입니다.

### 단계(step) 종류

**http**
```yaml
- http:
    name: login                     # 기본값: "METHOD /path"
    method: POST                    # 기본: GET (본문이 있으면 POST)
    url: "{{ baseUrl }}/login"
    headers: { X-Trace: "{{ uuid() }}" }
    query: { page: 1 }
    json: { id: "{{ users.id }}" }  # 또는 body: "..." 또는 form: {...}
    timeout: 5s
    check:
      status: 200                   # 200 | [200, 201] | "2xx". 생략 시 400 이상이면 실패
      bodyContains: ok
      json: { "$.token": "tok-{{ users.id }}" }
      header: { Content-Type: json }
      maxLatency: 500ms
    extract:
      token: $.token                # JSONPath
      session: header:Set-Cookie
      csrf: 'regex:name="csrf" value="([^"]+)"'
- http: "{{ baseUrl }}/health"      # 짧은 형태
```

**ws** — 연결 시간은 `<name>:connect`, 각 expect는 직전 send부터의 시간으로 `<name>:<expect name>`에 기록됩니다.
```yaml
- ws:
    name: chat
    url: "ws://localhost:8080/ws/chat"   # 생략하면 conn 이름으로 기존 연결 재사용
    keep: false                          # true면 반복이 끝나도 연결 유지
    headers: { Authorization: "Bearer {{ token }}" }
    actions:
      - sendJson: { type: join, room: "{{ room }}" }
      - expect: { name: joined, json: { "$.type": joined }, timeout: 2s, extract: { id: $.id } }
      - send: "plain text {{ vu }}"
      - expect: { contains: "plain text" }   # 맞지 않는 메시지는 건너뜀 (skipOthers: false로 끄기)
      - sleep: 1s
      - close: true
```

**grpc** — 단항(unary) 호출. 코드 생성이 필요 없습니다.
```yaml
- grpc:
    target: localhost:50051
    method: grpc.health.v1.Health/Check
    proto: proto/health.proto     # 또는 reflect: true (서버 리플렉션)
    importPaths: [proto]
    message: { service: "" }
    metadata: { authorization: "Bearer {{ token }}" }
    tls: false
    conns: 4                      # 호출을 나눌 HTTP/2 연결 수
    check: { code: OK, json: { "$.status": SERVING } }
    extract: { status: $.status }
```

**script** — `파일#함수`. VU마다 별도 JS 런타임이 있습니다.
```js
export function pickItem(ctx) {
  // ctx.vars(읽기/쓰기), ctx.last {status, body, headers, json, latencyMs}, ctx.vu, ctx.iter
  const items = ctx.last.json.items;
  if (!items.length) return false;          // false 반환 또는 fail("이유") → 반복 실패
  ctx.vars.itemId = items[0].id;
}
export function setup(ctx) {                // setup에서만 http.get/post/put/del/request 사용 가능
  const r = http.post(ctx.vars.baseUrl + "/login", { json: { id: "admin" } });
  return { adminToken: r.json().token };
}
```

**sleep**: `sleep: 500ms` 또는 `sleep: 500ms..2s`. **set**: `set: { page: "{{ randInt(1, 10) }}" }`.
아무 단계에나 `once: true`를 붙이면 VU당 첫 반복에서만 실행됩니다.

**sql** — DB에 직접 부하. `{{ }}`는 문자열로 이어 붙이지 않고 바인드 파라미터가 됩니다 (SQL 인젝션 안전).
```yaml
- sql:
    driver: postgres                # postgres | mysql
    dsn: "{{ pgUrl }}"
    query: SELECT id, name FROM users WHERE id = {{ users.id }} AND name LIKE '%{{ q }}%'
    exec: false                     # true = 행을 돌려받지 않는 INSERT/UPDATE
    check: { rows: ">= 1" }         # affected: 1, json: { "$[0].name": kim }
    extract: { id: row.0.id }
```
**redis**
```yaml
- redis:
    url: redis://:pass@localhost:6379/0
    command: [SET, "k:{{ vu }}", "{{ uuid() }}", EX, 60]   # 또는 commands: [[INCR, n], [GET, k]] (파이프라인)
    check: { reply: OK }
```
**graphql** — `errors` 배열이 오면 실패(`graphql:error`).
```yaml
- graphql:
    url: "{{ base }}/graphql"
    query: "query User($id: ID!) { user(id: $id) { id name } }"
    variables: { id: "{{ uid }}" }
    extract: { name: $.data.user.name }
```
**tcp** — 연결 시간(`<name>:connect`)과 왕복 시간을 따로 잽니다.
```yaml
- tcp: { addr: "localhost:6379", send: 'PING\r\n', expect: "+PONG", keep: true }
```
예시: `examples/protocols/`.

### 템플릿

`{{ 변수.경로 }}`, `{{ env.HOME }}`, `{{ vu }}`, `{{ iter }}`, `{{ scenario }}`, `{{ last.status }}`, `{{ last.json.x }}`,
함수 `uuid()`, `randInt(1, 100)`, `randFloat()`, `randStr(8)`, `now()`(ms), `isoNow()`.
문자열 전체가 `{{ x }}` 하나뿐이면 값의 타입(숫자 등)이 유지됩니다.

## 측정 방식

- **arrival-rate (open model)**: 응답을 기다리지 않고 정해진 시각에 반복을 시작합니다. 서버가 느려져도 보내는 양이 줄지 않으므로 지연이 그대로 드러납니다. 동시 진행이 `maxInFlight`에 닿으면 미루지 않고 **dropped**로 셉니다.
- **constant/ramping-users (closed model)**: 응답을 받아야 다음 요청을 보냅니다. 서버가 멈추면 요청도 같이 멈춰 지연이 가려지므로, 지연시간 목표 판정에는 arrival-rate를 쓰세요. (`examples/stall.yaml`로 차이를 직접 볼 수 있습니다.)
- **생성기 자체 감시**: 매초 CPU와 스케줄 지연(예정 시각보다 늦게 시작한 정도)을 기록하고, 측정을 믿기 어려우면 경고합니다.
- 지연시간은 요청 시작부터 응답 본문을 다 받을 때까지입니다. 연결 수립 시간도 포함됩니다.

## 히트맵과 액티브 트랜잭션

리포트와 실시간 대시보드 맨 위에 **히트맵**이 나옵니다. 가로는 시간, 세로는 응답시간(로그 눈금)이고, 그 칸에 들어간 요청이 많을수록 진하게 칠해집니다. 실패한 요청은 빨간색입니다.
위쪽으로 튀는 점들(느린 요청), 띠가 위로 올라가는 구간(전체가 느려짐), 빨간 점이 몰린 구간(에러)이 한눈에 보입니다.

- **드래그로 선택**: 영역을 끌면 그 안의 요청 목록(시각, 단계, 응답시간, 결과, trace ID)이 아래에 나옵니다. 초마다 최대 60개를 무작위로 남겨 두므로 "표본"입니다.
- **Server (APM)**: `--apm`을 켜면 서버가 보고한 트랜잭션으로 같은 히트맵을 그립니다. 위쪽 전환 버튼으로 바꿉니다.
- **트랜잭션 상세**: trace ID를 누르면 그 요청이 서버 안에서 쓴 시간을 SQL·외부 호출 단위의 막대로 보여줍니다 (APM 필요).

실시간 대시보드(`--dashboard`) 오른쪽에는 **액티브 트랜잭션**이 나옵니다. 지금 처리 중인 요청을 경과 시간별로(1초 미만 / 1–3초 / 3–8초 / 8초 이상) 세고, 오래 걸리는 순서로 어떤 단계(서버라면 어떤 SQL)에서 기다리는지 보여줍니다. 리포트에는 같은 값이 "In flight by age" 그래프로 남습니다.

## 알림 규칙

```yaml
alerts:
  - name: 결제 느림
    when: "latency{step:checkout} p95 > 500ms"   # 부하 지표: thresholds와 같은 문법
    for: 30s                                    # 30초 넘게 이어지면 알림 (기본: 즉시)
    window: 10s                                 # 최근 10초 기준으로 계산 (기본 10s)
    severity: critical                          # warning(기본) | critical
  - name: 서버 CPU
    when: "server{cpu} > 90"                    # monitor로 모은 서버 지표 (최근 window 평균)
  - name: 에러
    when: "errors rate > 1%"
    notify: ["https://hooks.slack.com/services/..."]   # 생략하면 --notify / $LT_NOTIFY
```
조건이 맞기 시작하면 "firing", 풀리면 "resolved"를 터미널과 웹훅으로 보냅니다. 테스트는 멈추지 않습니다(멈추려면 `limits`). 리포트의 "What happened"와 Alerts 탭에도 남습니다. `lt watch`에서도 똑같이 씁니다.

## APM: 서버 안에서 무슨 일이 있었나

`--apm :4318`(또는 시나리오의 `apm.listen`)을 켜면 lt가 테스트하는 동안 **서버가 보내는 트레이스**를 받습니다. lt가 요청마다 붙인 trace ID를 서버가 이어받기 때문에, 리포트의 요청 하나를 누르면 그 요청이 서버 안에서 어떤 SQL과 외부 호출에 얼마를 썼는지 막대로 펼쳐집니다.

| 서버 쪽 | 방법 |
|---|---|
| Go | lt 에이전트 (`github.com/ymg4798/lt/agent`, 표준 라이브러리만 사용) |
| Java, Python, Node.js, .NET 등 | OpenTelemetry SDK/에이전트를 그대로 쓰고 `OTEL_EXPORTER_OTLP_ENDPOINT=http://<lt 장비>:4318` (OTLP/HTTP, protobuf·JSON 모두) |

```go
a := agent.Start(agent.Config{Service: "shop", Endpoint: "http://localhost:4318"})
defer a.Shutdown(context.Background())

db, _ := agent.OpenDB("postgres", dsn)                 // SQL마다 span, 실행 중인 SQL은 액티브 트랜잭션에 표시
client := &http.Client{Transport: agent.Transport(nil)} // 외부 호출 span + trace 전달
http.ListenAndServe(":8080", agent.Middleware(mux))      // 들어온 요청 = 트랜잭션
```

리포트에 생기는 것:
- **히트맵 "Server (APM)"**: 서버 트랜잭션 기준 히트맵
- **Server operations 탭**: 엔드포인트, SQL(리터럴은 `?`로 정규화), 외부 호출별 횟수·평균·p95·총 시간
- **트랜잭션 상세**: trace ID를 누르면 서버 → SQL → 외부 호출 막대 그림. SQL 문장은 행을 누르면 펼쳐짐
- **서버 지표**: `apm tps`, `apm mean`(ms), `apm errors`
- **자동 분석**: "서버 시간의 70%가 SQL `SELECT ...`에 쓰임", "가장 느린 쿼리" 등
- **서비스 맵**: lt → 서비스 → DB·외부 호스트 연결도. 선 굵기는 호출 수, 빨간 선은 에러
- **Error groups**: 실패한 span·panic·에러 로그를 메시지별로 묶고, 예시 트레이스와 스택을 붙임
- **액티브 스택** (Go 에이전트): 1초 넘게 걸리는 요청은 지금 코드의 어느 줄에서 기다리는지 스택을 함께 보고. 대시보드 액티브 트랜잭션의 "stack"을 누르면 보임 (`Config.StackAfter`)
- 에이전트는 panic(→ 500과 스택), `span.Fail`, `slog` 로그(`agent.SlogHandler`, trace ID 포함)도 보냄

연습: `bin/testserver -apm http://localhost:4318 &` 후 `bin/lt run examples/apm.yaml --dashboard :5665`.
테스트 서버의 `/order/{id}`(SQL 3개 + 외부 호출, 5%는 느린 쿼리)와 `/report`(느린 집계 쿼리)가 트레이스를 보냅니다.

## 상시 감시: lt watch

부하 없이 서버·DB·APM을 계속 지켜보는 관제 화면입니다.

```bash
lt watch examples/watch.yaml          # http://localhost:5665
```
```yaml
name: shop
apm: { listen: ":4318" }
monitor:
  - url: http://localhost:8080/metrics
    name: api
  - postgres: "postgres://..."
alerts:
  - name: slow server
    when: "server{apm mean} > 50"     # ms
    severity: critical
```
화면에는 서버 지표 요약, 서버 트랜잭션 히트맵, 액티브 트랜잭션(지금 실행 중인 SQL까지), 서버 처리량과 응답시간, 서버 지표 그래프, Server operations, Top SQL, 알림 기록이 나옵니다.
알림은 `--notify`(또는 규칙의 `notify`)로 보냅니다. `--prometheus`로 같은 지표를 내보낼 수 있습니다.

**기록 보관**: 1초 기록을 `.lt/watch/<이름>/`에 시간 단위 파일로 저장합니다(`--data`, 기본 7일 보관 `--retention`). 다시 켜면 직전 기록부터 이어서 보이고, 화면 오른쪽 위에서 실시간 / 1시간 / 6시간 / 24시간 / 7일을 고르면 그 기간을 줄여서(점 600개) 보여줍니다. 서버 지표 20개 기준 하루 약 40MB, 7일 범위 조회는 0.1초 안쪽입니다.

## 결과 읽기: 자동 분석

실행이 끝나면 매초 기록을 읽어서 대상 서버에 대해 알아낸 것을 "findings"로 보여줍니다. 터미널, HTML 리포트, 마크다운 요약에 모두 나옵니다.

| 종류 | 언제 |
|---|---|
| 지연 급증 | p95가 초반의 2배(최소 +20ms) 이상으로 3초 넘게 유지될 때, 그 시점과 부하 |
| 에러 시작 | 에러율이 1%를 넘은 채로 2초 넘게 이어질 때, 가장 많은 에러 종류와 단계 |
| 처리 한계 | 동시 진행이 `maxInFlight`에 닿아 시작을 못 한 반복이 생긴 시점 |
| 처리량 정체 | (사용자 수 방식) 사용자를 1.5배 늘려도 처리량이 그대로일 때 |
| 순간 급증 | 특정 1초의 p99가 평소의 5배 이상 튈 때 (GC, 락 경합 의심) |
| 에러 집중 / 가장 느린 단계 | 에러의 80% 이상이 한 단계에서 나올 때, 요청 시간의 40% 이상을 한 단계가 차지할 때 |
| 서버 지표 | `monitor`를 켰다면, 문제가 시작된 시점 전후로 1.5배 이상 변한 서버 지표 |

리포트 그래프에는 이 시점들이 빨간 점선으로 표시됩니다.

## 한계 찾기

부하를 계속 올리면서 `limits`가 깨지는 순간 멈춥니다. 멈출 때 "마지막으로 모든 조건을 지킨 부하"를 알려주므로 그 값이 대략 서버가 버티는 최대치입니다.

```yaml
scenarios:
  ramp:
    stages: [{ duration: 5m, target: 5000 }]
    flow: [ ... ]
limits:
  latency: ["p95 < 300ms"]
  errors: ["rate < 1%"]
```
`examples/breakpoint.yaml`이 예시입니다. 서버 지표 수집과 트레이싱도 함께 켜져 있습니다.

## 서버 지표 같이 보기

`monitor`에 대상 서버의 Prometheus 엔드포인트나 프로세스 이름을 적으면, 부하와 같은 1초 단위로 기록되어 HTML 리포트의 "Server" 영역에 그래프로 나옵니다. Grafana 없이도 "언제 무엇이 올랐는지"를 한 화면에서 볼 수 있습니다.

- `metrics`를 생략하면 `process_cpu_seconds_total`(→ cpu %), `process_resident_memory_bytes`, `go_memstats_heap_inuse_bytes`, `go_goroutines` 중 있는 것을 씁니다.
- 이름이 `_total`, `_count`, `_sum`으로 끝나거나 `# TYPE`이 counter인 지표는 초당 증가량으로 바꿔서 보여줍니다.
- `name{label="v"}`, `!=`, `=~`, `!~`로 고를 수 있고, 맞는 시계열은 합칩니다. `headers`로 인증 헤더를 붙일 수 있습니다.
- 분산 실행에서는 컨트롤러(명령을 실행한 장비)가 수집합니다.

### 데이터베이스와 서버 (프리셋, 직접 연결)

```yaml
monitor:
  - postgres: "postgres://lt:lt@localhost:5432/app?sslmode=disable"   # 직접 연결
  - mysql: "user:pass@tcp(localhost:3306)/app"
  - redis: "redis://:pass@localhost:6379/0"                            # rediss:// 는 TLS
    name: cache                                   # 지표 이름 앞에 붙는 이름 (선택)
  - url: http://localhost:9100/metrics            # 이미 쓰는 exporter가 있으면
    preset: node                                  # node | mysql | postgres | redis
```

| 대상 | 1초마다 기록하는 것 |
|---|---|
| `postgres` | 연결 수, 실행 중 쿼리, tps, 커밋/롤백/데드락, 캐시 적중률, 락 대기, 가장 긴 쿼리·트랜잭션 |
| `mysql` | 연결 수, 실행 중 스레드, qps, 느린 쿼리, 행 락 대기, 버퍼 풀 적중률, 가장 긴 쿼리 |
| `redis` | 클라이언트, ops/s, 메모리, 적중률, 만료·퇴출 키, CPU |
| `preset: node` | CPU %, load1, 메모리, 디스크 읽기/쓰기, 네트워크 송수신, 디스크 사용률, TCP 연결 |

**컨테이너**: `- docker: local` (선택: `containers: ["web-*"]`)이면 Docker Engine API로 컨테이너별 CPU %, 메모리, 네트워크·디스크 초당 바이트, 재시작 횟수를 기록합니다. `tcp://host:2375`도 됩니다.

**Top SQL**: 직접 연결한 DB는 테스트 시작과 끝의 통계 차이로 "이 테스트 동안 시간을 가장 많이 쓴 쿼리" 20개를 리포트의 Top SQL 탭에 남깁니다.
PostgreSQL은 `pg_stat_statements` 확장, MySQL은 `performance_schema`가 켜져 있어야 하고(없으면 경고 후 나머지만 수집), Redis는 명령별 통계를 씁니다.
MySQL에서 서버 측 prepared statement(go-sql-driver의 기본 동작)로 실행된 쿼리는 집계되지 않습니다. `interpolateParams=true`면 잡힙니다.

연습용 DB와 exporter: `docker compose -f deploy/targets/docker-compose.yml up -d` (PostgreSQL 15432, MySQL 13306, Redis 16379, 계정 lt/lt). 예시는 `examples/database.yaml`입니다.

### 로그

```yaml
logs:
  - file: /var/log/app/*.log       # tail -F 처럼 따라감 (회전·잘림 처리)
    format: auto                   # auto | json | text
```
- JSON, logfmt, 일반 텍스트를 읽고 레벨·시각·trace ID·스택(Java, Python, Go)을 찾아냅니다. 여러 줄 스택은 한 기록으로 합칩니다.
- `--apm`을 켜면 OpenTelemetry 로그(`/v1/logs`)도 같은 포트로 받습니다. lt Go 에이전트는 `agent.SlogHandler`로 보냅니다.
- 리포트·대시보드의 **Logs 탭**에서 검색: 단어, `"구절"`, `/정규식/`, `level:error`, `trace:<id>`, `source:web-*`, 앞에 `-`면 제외.
- 트랜잭션 상세 창 아래에 **그 요청의 로그**가 함께 나옵니다.
- 같은 에러 로그는 메시지를 정규화해 **Error groups 탭**으로 묶습니다.
- 알림: `when: "log{level:error} > 10"` (최근 window 동안 줄 수).

## 분산 트레이싱

```yaml
options:
  tracing:
    propagator: w3c        # w3c(traceparent, 기본) | b3 | jaeger(uber-trace-id)
    sampled: true          # 서버에 "이 trace를 저장해 달라"고 요청 (기본 true)
    url: "http://localhost:16686/trace/{traceId}"   # 리포트에서 trace ID를 링크로
```
HTTP와 gRPC 요청마다 새 trace ID를 붙입니다. 서버가 OpenTelemetry 등으로 계측되어 있으면 그 요청이 서버 안에서 어디에 시간을 썼는지 Jaeger·Tempo·Zipkin에서 볼 수 있습니다.
리포트의 "Traced requests"에는 단계마다 가장 느린 요청 10개와 처음 실패한 요청 10개의 trace ID가 남습니다.

## 기록과 추이

```bash
lt run test.yaml --save                      # .lt/runs/<테스트 이름>/ 에 저장 (gzip)
lt run test.yaml --save --baseline last      # 직전 실행과 비교. p95/p99가 10% 넘게 느려지면 exit 1
lt history pin .lt/runs/test/20261101-161152.json.gz
lt run test.yaml --baseline pinned           # 지정한 기준 결과와 비교
lt history test --html trend.html            # 실행별 지연·처리량·에러율 추이
```

## CI와 알림

- **GitHub Actions**: `$GITHUB_STEP_SUMMARY`가 있으면 결과 표, 임계값, findings, 기준 비교를 실행 화면에 자동으로 붙입니다. 예시: `examples/ci/github-actions.yml`. 공개 저장소에서는 GitHub 기본 러너가 무료입니다.
- **알림**: `--notify <웹훅 URL>`. Slack(`hooks.slack.com`)과 Discord 웹훅은 각자의 형식으로, 그 외 URL에는 `{text, result}` JSON을 보냅니다. `--notify-on fail`이면 실패할 때만 보냅니다.
- **Grafana 구간 표시**: `--grafana http://localhost:3000`이면 테스트 시작~끝을 Grafana 주석(annotation)으로 남깁니다. 같은 Grafana의 모든 대시보드에서 "이 구간에 부하테스트가 있었다"가 보입니다. 토큰은 `$LT_GRAFANA_TOKEN`(서비스 계정 토큰, Editor 권한)입니다.

## 시나리오 만들기: 변환기

```bash
lt convert recording.har -o flow.yaml        # 브라우저 개발자도구 → Network → "Save all as HAR"
lt convert requests.txt                      # "Copy as cURL"로 복사한 명령들
lt convert openapi.yaml --rate 10 --duration 1m
```
- HAR: 이미지·CSS·JS 같은 정적 파일은 빼고(`--all`로 포함), 요청 사이 1초 이상 간격은 `sleep`으로 넣습니다. `--host`로 특정 호스트만 남길 수 있습니다.
- OpenAPI(3.x, Swagger 2.0): 작업마다 요청 하나를 만들고 예시 값이나 스키마로 본문을 채웁니다.
- 만들어진 파일 맨 위에 무엇을 건너뛰었는지 주석으로 남습니다. 토큰이나 ID는 그대로 들어가므로 `vars`, `data`, `extract`로 바꿔 주세요.

### 녹화: lt record

```bash
lt record --target http://localhost:8080 --out flow.yaml   # 앱이 localhost:8888 을 보게 하고 사용 → Ctrl+C
lt record --listen :8888 --mitm --host shop.example.com    # 브라우저 프록시로 설정 (HTTPS는 --mitm + 생성된 CA 신뢰)
```
끝나면 HAR 변환기와 같은 규칙(정적 파일 제외, 1초 넘는 간격은 sleep)으로 시나리오를 만듭니다. `--mitm`은 `~/.lt/ca`에 로컬 CA를 만들고 신뢰 방법을 안내합니다. 개인 키가 들어 있으니 다른 사람과 공유하지 마세요.

## 감시 모드

```bash
lt monitor test.yaml --every 1m --notify $SLACK_WEBHOOK --prometheus :9465
```
시나리오마다 흐름을 `--iterations`번(기본 1) 실행하고, 요청 실패나 임계값 실패가 있으면 FAIL입니다. 정상→실패, 실패→정상으로 바뀔 때만 알림을 보냅니다.
파일의 rate, stages, users는 무시하고, 점검마다 파일을 다시 읽습니다. `--prometheus`로 `lt_monitor_up` 같은 지표를 내보내면 Grafana에서 가용성을 볼 수 있습니다.

## 분산 실행

```bash
# 부하를 낼 각 장비에서
LT_TOKEN=secret lt agent --listen :7700
# 컨트롤러에서 (시나리오 폴더 전체를 에이전트로 보냄)
LT_TOKEN=secret lt run scenario.yaml --agents 10.0.0.11:7700,10.0.0.12:7700
```

- 도착률과 사용자 수를 에이전트 수로 나누고, 3초 뒤 동시에 시작합니다.
- `sequential`/`unique` 데이터 행은 에이전트끼리 겹치지 않게 나눕니다.
- 1초 단위 히스토그램을 합치므로 백분위수가 정확합니다(평균의 평균이 아닙니다).
- 에이전트는 스크립트를 실행하므로 `--token`을 꼭 설정하세요. 통신은 평문 HTTP이니 신뢰할 수 있는 네트워크에서만 쓰세요.

## Grafana

`lt`가 `/metrics`로 지표를 내놓으면 Prometheus가 1초마다 가져가 저장하고, Grafana가 그걸 그립니다.
내장 리포트/대시보드만으로 충분하면 필요 없고, **내 서비스 지표와 같은 화면에서 볼 때** 씁니다.

```bash
docker compose -f deploy/docker-compose.yml up -d
lt run scenario.yaml --prometheus :9464
open http://localhost:3000        # "lt load test" 대시보드
```

`deploy/prometheus.yml`에 대상 서비스를 추가하면 서버 지표와 부하 지표를 한 화면에서 볼 수 있습니다.
`--grafana http://localhost:3000`을 함께 쓰면 테스트 구간이 그래프 위에 표시됩니다 ("lt runs" 주석).
노출 지표: `lt_requests_total`, `lt_errors_total{kind}`, `lt_latency_seconds{quantile}`(최근 1초),
`lt_dropped_total`, `lt_in_flight`, `lt_users`, `lt_target_rate`, `lt_generator_cpu_percent`, `lt_scheduling_lag_seconds`.

## 많은 부하를 낼 때

`lt preflight`로 점검하세요. Apple M 계열 노트북 1대에서 대상 서버를 같은 장비에 띄운 상태로 초당 5만 건(CPU 43%)까지 확인했습니다.

- 열린 파일 수: `ulimit -n 65535`
- 포트 범위: keep-alive를 끄면(`noKeepAlive`) 연결마다 포트를 새로 써서 금방 부족해집니다.
- 그 이상은 `--agents`로 나누세요.

## 개발

```bash
make test     # go test -race ./...
make vet      # go vet + gofmt
make dist     # macOS/Linux/Windows 바이너리
```

구조와 설계 이유는 [docs/DESIGN.md](docs/DESIGN.md)에 있습니다.
Claude Code로 개발할 때 쓰는 에이전트·스킬·훅 구성은 [docs/HARNESS.md](docs/HARNESS.md)에 있습니다 (`/feature <요청>`으로 시작).
