package monitor

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/run"
	"github.com/ymg4798/lt/internal/scenario"
)

type val struct {
	unit string
	v    float64
}

// runPreset feeds two exporter pages one second apart through a preset.
func runPreset(t *testing.T, p presetFunc, first, second string) map[string]val {
	t.Helper()
	got := map[string]val{}
	prev := map[string]point{}
	t0 := time.Unix(1000, 0)
	for i, page := range []string{first, second} {
		ss, _, err := parseText(strings.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		c := newCalc(prev, t0.Add(time.Duration(i)*time.Second), func(n, u string, v float64) { got[n] = val{u, v} }, func(s string) string { return s })
		c.fam = groupSamples(ss)
		p(c)
	}
	return got
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6*math.Max(1, math.Abs(b)) }

func check(t *testing.T, got map[string]val, want map[string]val) {
	t.Helper()
	for n, w := range want {
		g, ok := got[n]
		if !ok {
			t.Errorf("%s missing (got %v)", n, got)
			continue
		}
		if g.unit != w.unit || !near(g.v, w.v) {
			t.Errorf("%s = %v %q, want %v %q", n, g.v, g.unit, w.v, w.unit)
		}
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			t.Errorf("unexpected series %s = %v", n, got[n])
		}
	}
}

const node1 = `# TYPE node_cpu_seconds_total counter
node_cpu_seconds_total{cpu="0",mode="idle"} 100
node_cpu_seconds_total{cpu="0",mode="user"} 50
node_cpu_seconds_total{cpu="1",mode="idle"} 100
node_cpu_seconds_total{cpu="1",mode="user"} 50
node_load1 1.5
node_memory_MemTotal_bytes 1000
node_memory_MemAvailable_bytes 250
node_disk_read_bytes_total{device="sda"} 1000
node_disk_read_bytes_total{device="dm-0"} 1000
node_disk_written_bytes_total{device="sda"} 0
node_network_receive_bytes_total{device="eth0"} 0
node_network_receive_bytes_total{device="lo"} 0
node_network_transmit_bytes_total{device="eth0"} 0
node_filesystem_size_bytes{device="/dev/sda1",fstype="ext4",mountpoint="/"} 100
node_filesystem_avail_bytes{device="/dev/sda1",fstype="ext4",mountpoint="/"} 40
node_filesystem_size_bytes{device="tmpfs",fstype="tmpfs",mountpoint="/run"} 1000
node_filesystem_avail_bytes{device="tmpfs",fstype="tmpfs",mountpoint="/run"} 1000
node_netstat_Tcp_CurrEstab 12
`

func TestNodePreset(t *testing.T) {
	// One second later: 1.5 of 2 cores idle → 25% busy.
	node2 := strings.NewReplacer(
		`cpu="0",mode="idle"} 100`, `cpu="0",mode="idle"} 100.75`,
		`cpu="1",mode="idle"} 100`, `cpu="1",mode="idle"} 100.75`,
		`device="sda"} 1000`, `device="sda"} 5096`,
		`device="dm-0"} 1000`, `device="dm-0"} 9000`,
		`written_bytes_total{device="sda"} 0`, `written_bytes_total{device="sda"} 10`,
		`receive_bytes_total{device="eth0"} 0`, `receive_bytes_total{device="eth0"} 300`,
		`receive_bytes_total{device="lo"} 0`, `receive_bytes_total{device="lo"} 99999`,
		`transmit_bytes_total{device="eth0"} 0`, `transmit_bytes_total{device="eth0"} 200`,
	).Replace(node1)
	check(t, runPreset(t, nodePreset, node1, node2), map[string]val{
		"cpu":              {"%", 25},
		"load1":            {"", 1.5},
		"memory used":      {"%", 75},
		"memory":           {"bytes", 750},
		"disk read bytes":  {"/s", 4096},
		"disk write bytes": {"/s", 10},
		"net rx bytes":     {"/s", 300},
		"net tx bytes":     {"/s", 200},
		"disk used":        {"%", 60},
		"tcp established":  {"", 12},
	})
}

func TestNodePresetLargestFilesystem(t *testing.T) {
	page := `node_filesystem_size_bytes{fstype="ext4",mountpoint="/boot"} 10
node_filesystem_avail_bytes{fstype="ext4",mountpoint="/boot"} 10
node_filesystem_size_bytes{fstype="xfs",mountpoint="/data"} 200
node_filesystem_avail_bytes{fstype="xfs",mountpoint="/data"} 50
`
	check(t, runPreset(t, nodePreset, page, page), map[string]val{"disk used": {"%", 75}})
}

func TestMySQLPreset(t *testing.T) {
	p1 := `mysql_global_status_threads_connected 10
mysql_global_status_threads_running 3
mysql_global_status_questions 1000
mysql_global_status_queries 5000
mysql_global_status_commands_total{command="commit"} 10
mysql_global_status_commands_total{command="rollback"} 1
mysql_global_status_commands_total{command="select"} 900
mysql_global_status_slow_queries 2
mysql_global_status_innodb_row_lock_waits 5
mysql_global_status_innodb_buffer_pool_read_requests 10000
mysql_global_status_innodb_buffer_pool_reads 100
mysql_global_status_aborted_connects 0
`
	p2 := strings.NewReplacer("questions 1000", "questions 1500", `"commit"} 10`, `"commit"} 30`,
		"slow_queries 2", "slow_queries 3", "lock_waits 5", "lock_waits 9",
		"read_requests 10000", "read_requests 11000", "pool_reads 100", "pool_reads 110",
		"aborted_connects 0", "aborted_connects 2").Replace(p1)
	check(t, runPreset(t, mysqlPreset, p1, p2), map[string]val{
		"mysql connections":      {"", 10},
		"mysql threads running":  {"", 3},
		"mysql qps":              {"/s", 500},
		"mysql commits":          {"/s", 20},
		"mysql rollbacks":        {"/s", 0},
		"mysql slow queries":     {"/s", 1},
		"mysql row lock waits":   {"/s", 4},
		"mysql buffer pool hit":  {"%", 99}, // 1000 logical reads, 10 from disk
		"mysql aborted connects": {"/s", 2},
	})
}

func TestPostgresPreset(t *testing.T) {
	p1 := `pg_stat_database_numbackends{datid="1",datname="app"} 7
pg_stat_database_numbackends{datid="2",datname="postgres"} 1
pg_stat_activity_count{datname="app",state="active"} 2
pg_stat_activity_count{datname="app",state="idle"} 5
pg_stat_database_xact_commit{datname="app"} 100
pg_stat_database_xact_rollback{datname="app"} 10
pg_stat_database_deadlocks{datname="app"} 0
pg_stat_database_blks_hit{datname="app"} 1000
pg_stat_database_blks_read{datname="app"} 100
pg_stat_database_tup_returned{datname="app"} 0
pg_stat_database_tup_fetched{datname="app"} 0
pg_stat_activity_max_tx_duration{datname="app",state="active"} 0.5
pg_stat_activity_max_tx_duration{datname="app",state="idle in transaction"} 2.25
pg_replication_is_replica{server="db:5432"} 0
pg_replication_lag_seconds{server="db:5432"} 0
`
	p2 := strings.NewReplacer("xact_commit{datname=\"app\"} 100", "xact_commit{datname=\"app\"} 190",
		"xact_rollback{datname=\"app\"} 10", "xact_rollback{datname=\"app\"} 20",
		"blks_hit{datname=\"app\"} 1000", "blks_hit{datname=\"app\"} 1090",
		"blks_read{datname=\"app\"} 100", "blks_read{datname=\"app\"} 110",
		"tup_returned{datname=\"app\"} 0", "tup_returned{datname=\"app\"} 50",
		"tup_fetched{datname=\"app\"} 0", "tup_fetched{datname=\"app\"} 40").Replace(p1)
	check(t, runPreset(t, postgresPreset, p1, p2), map[string]val{
		"pg connections":         {"", 8},
		"pg active":              {"", 2},
		"pg tps":                 {"/s", 100},
		"pg commits":             {"/s", 90},
		"pg rollbacks":           {"/s", 10},
		"pg deadlocks":           {"/s", 0},
		"pg cache hit":           {"%", 90},
		"pg rows returned":       {"/s", 50},
		"pg rows fetched":        {"/s", 40},
		"pg longest transaction": {"ms", 2250},
	})
}

func TestRedisPreset(t *testing.T) {
	p1 := `redis_connected_clients 4
redis_commands_processed_total 1000
redis_memory_used_bytes 1048576
redis_keyspace_hits_total 10
redis_keyspace_misses_total 10
redis_evicted_keys_total 0
redis_blocked_clients 1
redis_db_keys{db="db0"} 5
redis_db_keys{db="db1"} 6
`
	p2 := strings.NewReplacer("processed_total 1000", "processed_total 3000", "hits_total 10", "hits_total 40",
		"misses_total 10", "misses_total 20").Replace(p1)
	check(t, runPreset(t, redisPreset, p1, p2), map[string]val{
		"redis clients":         {"", 4},
		"redis ops":             {"/s", 2000},
		"redis memory":          {"bytes", 1048576},
		"redis hit":             {"%", 75},
		"redis evicted keys":    {"/s", 0},
		"redis blocked clients": {"", 1},
		"redis keys":            {"", 11},
	})
}

func TestPresetMissingMetricsAndCounterReset(t *testing.T) {
	// An unrelated page yields nothing; a counter that goes down yields no rate.
	if got := runPreset(t, nodePreset, "foo 1\n", "foo 2\n"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	got := runPreset(t, redisPreset, "redis_commands_processed_total 100\n", "redis_commands_processed_total 5\n")
	if _, ok := got["redis ops"]; ok {
		t.Fatalf("rate after reset: %v", got)
	}
}

func TestPresetScrapeWithExtraMetrics(t *testing.T) {
	var a, b atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := &a
		if r.URL.Path == "/b" {
			n = &b
		}
		c := n.Add(1000)
		fmt.Fprintf(w, "redis_connected_clients 3\nredis_commands_processed_total %d\nredis_uptime_in_seconds 99\n", c)
	}))
	defer srv.Close()
	m, err := New([]scenario.MonitorTarget{
		{URL: srv.URL, Preset: "redis", Metrics: []string{"redis_uptime_in_seconds"}},
		{URL: srv.URL + "/b", Name: "cache", Preset: "redis"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	time.Sleep(2300 * time.Millisecond)
	m.Close()
	s := &metrics.Snapshot{}
	m.Decorate(s)
	host := strings.TrimPrefix(srv.URL, "http://")
	if s.Server[host+" redis clients"] != 3 || s.Server[host+" redis_uptime_in_seconds"] != 99 || s.Server["cache redis clients"] != 3 {
		t.Fatalf("server: %v", s.Server)
	}
	if r := s.Server["cache redis ops"]; r < 700 || r > 1300 {
		t.Fatalf("ops: %v", s.Server)
	}
	if _, ok := s.Server["cpu"]; ok {
		t.Fatal("preset targets should not add the default metrics")
	}
	if _, err := New([]scenario.MonitorTarget{{URL: srv.URL, Preset: "oracle"}}, nil); err == nil {
		t.Fatal("unknown preset should fail")
	}
}

func TestTopDelta(t *testing.T) {
	base := map[string]stmtStat{
		"a": {query: "SELECT 1", calls: 10, totalMs: 100, rows: 10},
		"b": {query: "SELECT b", calls: 5, totalMs: 50},
		"c": {query: "SELECT c", calls: 100, totalMs: 1000},
		"d": {query: "UPDATE d", calls: 3, totalMs: 9},
	}
	cur := map[string]stmtStat{
		"a": {query: "SELECT 1", calls: 30, totalMs: 400, rows: 30}, // +20 calls, +300ms
		"b": {query: "SELECT b", calls: 5, totalMs: 50},             // idle: dropped
		"c": {query: "SELECT c", calls: 2, totalMs: 7},              // reset: count all
		"e": {query: "INSERT\n   INTO e", calls: 1, totalMs: 500},   // new
	}
	got := topDelta("pg", base, cur, 20)
	want := []run.TopQuery{
		{Source: "pg", Query: "INSERT INTO e", Calls: 1, TotalMs: 500, MeanMs: 500},
		{Source: "pg", Query: "SELECT 1", Calls: 20, TotalMs: 300, MeanMs: 15, Rows: 20},
		{Source: "pg", Query: "SELECT c", Calls: 2, TotalMs: 7, MeanMs: 3.5},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if got := topDelta("pg", base, cur, 1); len(got) != 1 || got[0].Query != "INSERT INTO e" {
		t.Fatalf("limit: %+v", got)
	}
	long := strings.Repeat("é", 800)
	if q := cleanQuery(long); len(q) > maxQueryLen+len("…") || !strings.HasSuffix(q, "…") {
		t.Fatalf("clean %d", len(q))
	}
}

func TestMonitorQueryFilter(t *testing.T) {
	for q, want := range map[string]bool{
		"SHOW GLOBAL STATUS WHERE `Variable_name` IN (...)":                          true,
		"SELECT COALESCE ( `SCHEMA_NAME` , ? ) FROM `performance_schema` . `events`": true,
		"SELECT * FROM `orders` WHERE `id` = ?":                                      false,
	} {
		if isMonitorSQL(q) != want {
			t.Errorf("%q: %v", q, !want)
		}
	}
}

func TestParseMySQLDSN(t *testing.T) {
	c, err := parseMySQLDSN("mysql://u:p%40ss@db.local/app?tls=skip-verify")
	if err != nil || c.User != "u" || c.Passwd != "p@ss" || c.Addr != "db.local:3306" || c.DBName != "app" || c.TLSConfig != "skip-verify" {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = parseMySQLDSN("lt:lt@tcp(localhost:13306)/lt")
	if err != nil || c.Addr != "localhost:13306" || c.User != "lt" {
		t.Fatalf("%+v %v", c, err)
	}
}

// ---- Redis ----

func TestRESP(t *testing.T) {
	var b strings.Builder
	if err := writeCommand(&b, "AUTH", "user", "pa ss"); err != nil {
		t.Fatal(err)
	}
	if b.String() != "*3\r\n$4\r\nAUTH\r\n$4\r\nuser\r\n$5\r\npa ss\r\n" {
		t.Fatalf("%q", b.String())
	}
	in := "+OK\r\n-ERR bad\r\n:42\r\n$5\r\nhe\r\no\r\n$-1\r\n*3\r\n:1\r\n$1\r\nx\r\n-WRONG\r\n*0\r\n"
	r := bufio.NewReader(strings.NewReader(in))
	expect := []string{"OK", "err:ERR bad", "42", "he\r\no", "<nil>", "err:WRONG", "[]"}
	for _, w := range expect {
		v, err := readReply(r)
		got := fmt.Sprint(v)
		if err != nil {
			got = "err:" + err.Error()
			if !isRedisError(err) {
				t.Fatalf("not a redis error: %v", err)
			}
		}
		if got != w {
			t.Fatalf("got %q want %q", got, w)
		}
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader("?x\r\n"))); err == nil {
		t.Fatal("bad type should fail")
	}
}

func TestParseRedisURL(t *testing.T) {
	for in, want := range map[string]string{
		"redis://:secret@cache:6380/2":     "cache:6380|/secret|2|false",
		"redis://bob:pw@localhost":         "localhost:6379|bob/pw|0|false",
		"rediss://h:7000?insecure=true":    "h:7000|/|0|true",
		"localhost:16379":                  "localhost:16379|/|0|false",
		"redis://onlypassword@host:1/0":    "host:1|/onlypassword|0|false",
		"rediss://:x@secure.example.com/1": "secure.example.com:6379|/x|1|false",
	} {
		s, _, err := newRedis(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		got := fmt.Sprintf("%s|%s/%s|%d|%v", s.addr, s.user, s.pass, s.db, s.tls != nil && s.tls.InsecureSkipVerify)
		if got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if _, _, err := newRedis("http://x"); err == nil {
		t.Error("http scheme should fail")
	}
}

const redisInfo1 = "# Server\r\nredis_version:7.4.5\r\nrole:master\r\n\r\n# Clients\r\nconnected_clients:3\r\nblocked_clients:0\r\n" +
	"# Memory\r\nused_memory:2048\r\n# Stats\r\ntotal_commands_processed:100\r\nkeyspace_hits:0\r\nkeyspace_misses:0\r\n" +
	"evicted_keys:0\r\nexpired_keys:0\r\n# CPU\r\nused_cpu_sys:1.0\r\nused_cpu_user:1.0\r\n# Keyspace\r\ndb0:keys=10,expires=1,avg_ttl=0\r\ndb3:keys=5,expires=0,avg_ttl=0\r\n"

const redisCmdStats1 = "# Commandstats\r\ncmdstat_get:calls=10,usec=100,usec_per_call=10.00,rejected_calls=0,failed_calls=0\r\n" +
	"cmdstat_info:calls=50,usec=5000,usec_per_call=100.00\r\ncmdstat_config|get:calls=1,usec=30,usec_per_call=30.00\r\n"

// fakeRedis answers AUTH, SELECT, CLIENT and INFO; every INFO adds traffic.
func fakeRedis(t *testing.T, pass string) (addr string, cmds *atomic.Int64) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cmds = &atomic.Int64{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				rd := bufio.NewReader(c)
				authed := pass == ""
				for {
					v, err := readReply(rd)
					if err != nil {
						return
					}
					args := v.([]any)
					cmd := strings.ToUpper(args[0].(string))
					n := cmds.Add(1)
					bulk := func(s string) { fmt.Fprintf(c, "$%d\r\n%s\r\n", len(s), s) }
					switch {
					case cmd == "AUTH":
						if args[len(args)-1] == pass {
							authed = true
							fmt.Fprint(c, "+OK\r\n")
						} else {
							fmt.Fprint(c, "-WRONGPASS invalid password\r\n")
						}
					case !authed:
						fmt.Fprint(c, "-NOAUTH Authentication required.\r\n")
					case cmd == "SELECT", cmd == "CLIENT":
						fmt.Fprint(c, "+OK\r\n")
					case cmd == "INFO" && len(args) > 1:
						bulk(strings.Replace(redisCmdStats1, "calls=10,usec=100", fmt.Sprintf("calls=%d,usec=%d", 10+n, 100+n*20), 1))
					case cmd == "INFO":
						bulk(strings.NewReplacer("total_commands_processed:100", fmt.Sprintf("total_commands_processed:%d", 100*n),
							"keyspace_hits:0", fmt.Sprintf("keyspace_hits:%d", 3*n), "keyspace_misses:0", fmt.Sprintf("keyspace_misses:%d", n)).Replace(redisInfo1))
					default:
						fmt.Fprint(c, "-ERR unknown command\r\n")
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), cmds
}

func TestParseInfoAndCommandStats(t *testing.T) {
	in := parseInfo(redisInfo1)
	if in["redis_version"] != "7.4.5" || in["db3"] != "keys=5,expires=0,avg_ttl=0" {
		t.Fatalf("%v", in)
	}
	st := parseCommandStats(parseInfo(redisCmdStats1))
	if len(st) != 2 || st["get"].calls != 10 || st["get"].totalMs != 0.1 || st["config|get"].query != "CONFIG|GET" {
		t.Fatalf("%+v", st)
	}
}

func TestRedisSourceAgainstFake(t *testing.T) {
	addr, _ := fakeRedis(t, "pw")
	bad, _, _ := newRedis("redis://:nope@" + addr)
	if err := bad.health(context.Background(), newCalc(map[string]point{}, time.Now(), func(string, string, float64) {}, func(s string) string { return s })); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("bad password: %v", err)
	}
	m, err := New([]scenario.MonitorTarget{{Name: "cache", Redis: "redis://:{{ pw }}@" + addr + "/3"}}, map[string]any{"pw": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	time.Sleep(2200 * time.Millisecond)
	top := m.TopSQL()
	s := &metrics.Snapshot{}
	m.Decorate(s)
	r := &run.Result{TopSQL: []run.TopQuery{{Source: "x", Query: "other", Calls: 1, TotalMs: 1e9}}}
	m.Finish(r)
	m.Close()
	m.Close() // twice is fine
	if s.Server["cache redis clients"] != 2 || s.Server["cache redis memory"] != 2048 || s.Server["cache redis keys"] != 15 {
		t.Fatalf("server %v", s.Server)
	}
	if h := s.Server["cache redis hit"]; h != 75 {
		t.Fatalf("hit %v", s.Server)
	}
	if ops := s.Server["cache redis ops"]; ops <= 0 {
		t.Fatalf("ops %v", s.Server)
	}
	if len(top) != 1 || top[0].Source != "cache" || top[0].Query != "GET" || top[0].Calls <= 0 || top[0].TotalMs <= 0 {
		t.Fatalf("top %+v", top)
	}
	if len(r.TopSQL) != 2 || r.TopSQL[0].Query != "other" || r.TopSQL[1].Query != "GET" {
		t.Fatalf("finish %+v", r.TopSQL)
	}
	units := map[string]string{}
	for _, x := range m.Series() {
		units[x.Name] = x.Unit
	}
	if units["cache redis ops"] != "/s" || units["cache redis memory"] != "bytes" || units["cache redis hit"] != "%" {
		t.Fatalf("units %v", units)
	}
}

func TestDBTargetNames(t *testing.T) {
	m, err := New([]scenario.MonitorTarget{
		{Postgres: "postgres://u:p@db1:5432/app"},
		{Postgres: "postgres://u:p@db2:5433/app"},
		{MySQL: "u:p@tcp(my:3306)/app"},
		{Redis: "redis://cache:6379"},
		{URL: "http://exp:9100/metrics", Preset: "node"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var got []string
	for _, tg := range m.targets {
		got = append(got, tg.source+"|"+tg.name("x"))
	}
	want := "[postgres db1:5432|db1:5432 x postgres db2:5433|db2:5433 x mysql|x redis|x |exp:9100 x]"
	if fmt.Sprint(got) != want {
		t.Fatalf("%v\nwant %s", got, want)
	}
	if _, err := New([]scenario.MonitorTarget{{Postgres: "::bad::"}}, nil); err == nil {
		t.Fatal("bad dsn should fail")
	}
}
