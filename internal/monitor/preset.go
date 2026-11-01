package monitor

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// calc turns raw totals into the friendly series of a preset or database:
// rates from counters, hit ratios over the last interval, sums across labels.
// Its state lives on the target and is touched only by its scrape goroutine.
type calc struct {
	now   time.Time
	prev  map[string]point
	fam   map[string][]sample // Prometheus samples by metric name (presets)
	emit  func(name, unit string, v float64)
	named func(string) string
}

type point struct {
	v  float64
	at time.Time
}

func newCalc(prev map[string]point, now time.Time, emit func(name, unit string, v float64), named func(string) string) *calc {
	return &calc{now: now, prev: prev, emit: emit, named: named}
}

// gauge reports v as is.
func (c *calc) gauge(name, unit string, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	c.emit(c.named(name), unit, v)
}

// delta returns how much the counter key grew since the previous call and
// over how many seconds; false on the first call or after a reset.
func (c *calc) delta(key string, total float64) (d, secs float64, ok bool) {
	p, had := c.prev[key]
	c.prev[key] = point{total, c.now}
	if !had || total < p.v {
		return 0, 0, false
	}
	secs = c.now.Sub(p.at).Seconds()
	if secs <= 0 {
		return 0, 0, false
	}
	return total - p.v, secs, true
}

// rate reports a counter as per second (unit "/s").
func (c *calc) rate(name string, total float64) {
	if d, s, ok := c.delta(name, total); ok {
		c.gauge(name, "/s", d/s)
	}
}

// hitRatio reports hits/(hits+misses) over the last interval, in %. With
// no traffic in the interval nothing is reported (the last value stays).
func (c *calc) hitRatio(name string, hits, misses float64) {
	dh, _, ok1 := c.delta(name+"\x00hit", hits)
	dm, _, ok2 := c.delta(name+"\x00miss", misses)
	if ok1 && ok2 && dh+dm > 0 {
		c.gauge(name, "%", dh/(dh+dm)*100)
	}
}

// ---- Prometheus sample helpers ----

func groupSamples(ss []sample) map[string][]sample {
	m := map[string][]sample{}
	for _, s := range ss {
		if !math.IsNaN(s.value) {
			m[s.name] = append(m[s.name], s)
		}
	}
	return m
}

// sum adds the samples of a metric that pass keep (nil: all).
func (c *calc) sum(name string, keep func(map[string]string) bool) (float64, bool) {
	var v float64
	found := false
	for _, s := range c.fam[name] {
		if keep == nil || keep(s.labels) {
			v += s.value
			found = true
		}
	}
	return v, found
}

func (c *calc) max(name string, keep func(map[string]string) bool) (float64, bool) {
	v, found := math.Inf(-1), false
	for _, s := range c.fam[name] {
		if keep == nil || keep(s.labels) {
			v = math.Max(v, s.value)
			found = true
		}
	}
	return v, found
}

func (c *calc) gaugeOf(display, unit, metric string, keep func(map[string]string) bool) {
	if v, ok := c.sum(metric, keep); ok {
		c.gauge(display, unit, v)
	}
}

func (c *calc) rateOf(display, metric string, keep func(map[string]string) bool) {
	if v, ok := c.sum(metric, keep); ok {
		c.rate(display, v)
	}
}

func label(k, v string) func(map[string]string) bool {
	return func(l map[string]string) bool { return l[k] == v }
}

// ---- presets ----

type presetFunc func(c *calc)

var presets = map[string]presetFunc{
	"node":     nodePreset,
	"mysql":    mysqlPreset,
	"postgres": postgresPreset,
	"redis":    redisPreset,
}

func lookupPreset(name string) (presetFunc, error) {
	p, ok := presets[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("unknown preset %q (node, mysql, postgres, redis)", name)
	}
	return p, nil
}

// Devices that would count the same bytes twice or are not real traffic.
var (
	virtualNICs  = []string{"lo", "veth", "docker", "br-", "virbr", "cni", "flannel", "cali", "tunl", "vxlan", "kube-", "lxc"}
	virtualDisks = []string{"loop", "ram", "zram", "sr", "dm-", "md", "fd"}
	pseudoFS     = map[string]bool{"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true, "ramfs": true, "nsfs": true, "autofs": true, "rootfs": false}
)

func notPrefixed(key string, prefixes []string) func(map[string]string) bool {
	return func(l map[string]string) bool {
		v := l[key]
		for _, p := range prefixes {
			if strings.HasPrefix(v, p) {
				return false
			}
		}
		return true
	}
}

// nodePreset reads node_exporter (Linux; CPU and load also on macOS).
func nodePreset(c *calc) {
	// CPU busy across all cores: 1 - idle seconds per second / cores.
	cpus := map[string]bool{}
	var idle float64
	for _, s := range c.fam["node_cpu_seconds_total"] {
		if s.labels["mode"] == "idle" {
			idle += s.value
			cpus[s.labels["cpu"]] = true
		}
	}
	if len(cpus) > 0 {
		if d, secs, ok := c.delta("cpu\x00idle", idle); ok {
			busy := (1 - d/secs/float64(len(cpus))) * 100
			c.gauge("cpu", "%", math.Max(0, math.Min(100, busy)))
		}
	}
	c.gaugeOf("load1", "", "node_load1", nil)

	total, ok1 := c.sum("node_memory_MemTotal_bytes", nil)
	avail, ok2 := c.sum("node_memory_MemAvailable_bytes", nil)
	if ok1 && ok2 && total > 0 {
		c.gauge("memory used", "%", (total-avail)/total*100)
		c.gauge("memory", "bytes", total-avail)
	}

	disk := notPrefixed("device", virtualDisks)
	c.rateOf("disk read bytes", "node_disk_read_bytes_total", disk)
	c.rateOf("disk write bytes", "node_disk_written_bytes_total", disk)
	nic := notPrefixed("device", virtualNICs)
	c.rateOf("net rx bytes", "node_network_receive_bytes_total", nic)
	c.rateOf("net tx bytes", "node_network_transmit_bytes_total", nic)

	// Filesystem: "/" if exported, else the largest real one.
	var size, free float64
	var mount string
	for _, s := range c.fam["node_filesystem_size_bytes"] {
		mp := s.labels["mountpoint"]
		if pseudoFS[s.labels["fstype"]] && mp != "/" {
			continue
		}
		if mount != "/" && (mp == "/" || s.value > size) {
			size, mount = s.value, mp
		}
	}
	if mount != "" && size > 0 {
		if v, ok := c.sum("node_filesystem_avail_bytes", label("mountpoint", mount)); ok {
			free = v
			c.gauge("disk used", "%", (size-free)/size*100)
		}
	}
	c.gaugeOf("tcp established", "", "node_netstat_Tcp_CurrEstab", nil)
}

// mysqlPreset reads mysqld_exporter (global status is exported untyped,
// so counters are turned into rates here).
func mysqlPreset(c *calc) {
	c.gaugeOf("mysql connections", "", "mysql_global_status_threads_connected", nil)
	c.gaugeOf("mysql threads running", "", "mysql_global_status_threads_running", nil)
	if v, ok := c.sum("mysql_global_status_questions", nil); ok {
		c.rate("mysql qps", v)
	} else {
		c.rateOf("mysql qps", "mysql_global_status_queries", nil)
	}
	c.rateOf("mysql commits", "mysql_global_status_commands_total", label("command", "commit"))
	c.rateOf("mysql rollbacks", "mysql_global_status_commands_total", label("command", "rollback"))
	c.rateOf("mysql slow queries", "mysql_global_status_slow_queries", nil)
	c.rateOf("mysql row lock waits", "mysql_global_status_innodb_row_lock_waits", nil)
	req, ok1 := c.sum("mysql_global_status_innodb_buffer_pool_read_requests", nil)
	disk, ok2 := c.sum("mysql_global_status_innodb_buffer_pool_reads", nil)
	if ok1 && ok2 {
		// read_requests counts every logical read; reads are the misses.
		c.hitRatio("mysql buffer pool hit", math.Max(0, req-disk), disk)
	}
	c.rateOf("mysql aborted connects", "mysql_global_status_aborted_connects", nil)
	if v, ok := c.max("mysql_slave_status_seconds_behind_master", nil); ok {
		c.gauge("mysql replication lag", "ms", v*1000)
	}
}

// postgresPreset reads postgres_exporter.
func postgresPreset(c *calc) {
	c.gaugeOf("pg connections", "", "pg_stat_database_numbackends", nil)
	c.gaugeOf("pg active", "", "pg_stat_activity_count", label("state", "active"))
	commit, ok1 := c.sum("pg_stat_database_xact_commit", nil)
	rollback, ok2 := c.sum("pg_stat_database_xact_rollback", nil)
	if ok1 && ok2 {
		c.rate("pg tps", commit+rollback)
	}
	if ok1 {
		c.rate("pg commits", commit)
	}
	if ok2 {
		c.rate("pg rollbacks", rollback)
	}
	c.rateOf("pg deadlocks", "pg_stat_database_deadlocks", nil)
	hit, ok1 := c.sum("pg_stat_database_blks_hit", nil)
	read, ok2 := c.sum("pg_stat_database_blks_read", nil)
	if ok1 && ok2 {
		c.hitRatio("pg cache hit", hit, read)
	}
	c.rateOf("pg rows returned", "pg_stat_database_tup_returned", nil)
	c.rateOf("pg rows fetched", "pg_stat_database_tup_fetched", nil)
	if v, ok := c.max("pg_stat_activity_max_tx_duration", nil); ok {
		c.gauge("pg longest transaction", "ms", v*1000)
	}
	// Lag is exported as 0 on a primary too; show it only on replicas.
	if r, ok := c.max("pg_replication_is_replica", nil); !ok || r > 0 {
		if v, ok := c.max("pg_replication_lag_seconds", nil); ok {
			c.gauge("pg replication lag", "ms", v*1000)
		} else if v, ok := c.max("pg_replication_lag", nil); ok {
			c.gauge("pg replication lag", "ms", v*1000)
		}
	}
}

// redisPreset reads redis_exporter.
func redisPreset(c *calc) {
	c.gaugeOf("redis clients", "", "redis_connected_clients", nil)
	c.rateOf("redis ops", "redis_commands_processed_total", nil)
	c.gaugeOf("redis memory", "bytes", "redis_memory_used_bytes", nil)
	hits, ok1 := c.sum("redis_keyspace_hits_total", nil)
	miss, ok2 := c.sum("redis_keyspace_misses_total", nil)
	if ok1 && ok2 {
		c.hitRatio("redis hit", hits, miss)
	}
	c.rateOf("redis evicted keys", "redis_evicted_keys_total", nil)
	c.gaugeOf("redis blocked clients", "", "redis_blocked_clients", nil)
	c.gaugeOf("redis keys", "", "redis_db_keys", nil)
}
