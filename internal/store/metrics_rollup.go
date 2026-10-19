package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Metrics arrive about every 10 seconds. They are kept at that resolution
// for RawMetricsKept; older samples are rolled up into one sample per
// MetricsBucket, kept for MetricsKept.
const (
	MetricsBucket  = 5 * time.Minute
	RawMetricsKept = 6 * time.Hour
	MetricsKept    = 30 * 24 * time.Hour
)

const settingMetricsRolledUp = "metrics_rolled_up_until"

// metricsRollup describes how the samples of a table merge into one:
// gauges are averaged, totals, limits and counters (cumulative traffic,
// uptime) take their highest value.
var metricsRollups = []struct {
	table, key string
	cols, aggs []string
}{
	{
		table: "server_metrics", key: "server_id",
		cols: []string{"cpu_pct", "mem_used", "mem_total", "disk_used", "disk_total", "load1", "load5", "load15", "uptime"},
		aggs: []string{"AVG(cpu_pct)", "CAST(ROUND(AVG(mem_used)) AS INTEGER)", "MAX(mem_total)",
			"CAST(ROUND(AVG(disk_used)) AS INTEGER)", "MAX(disk_total)", "AVG(load1)", "AVG(load5)", "AVG(load15)", "MAX(uptime)"},
	},
	{
		table: "app_metrics", key: "app_id",
		cols: []string{"cpu_pct", "mem_used", "mem_limit", "net_rx", "net_tx"},
		aggs: []string{"AVG(cpu_pct)", "CAST(ROUND(AVG(mem_used)) AS INTEGER)", "MAX(mem_limit)", "MAX(net_rx)", "MAX(net_tx)"},
	},
}

// RollUpMetrics replaces the samples older than RawMetricsKept (up to the
// start of a bucket) with one sample per bucket, stamped with the bucket's
// start. Samples rolled up before are left alone. It returns how many
// samples were replaced.
func (db *DB) RollUpMetrics(ctx context.Context, now time.Time) (int64, error) {
	bucket := int64(MetricsBucket / time.Second)
	until := now.Add(-RawMetricsKept).Unix() / bucket * bucket
	var from int64
	if v, err := db.Setting(ctx, settingMetricsRolledUp); err == nil {
		from, _ = strconv.ParseInt(v, 10, 64)
	} else if !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	if until <= from {
		return 0, nil
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var replaced int64
	for _, r := range metricsRollups {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT %s, at / %d * %d AS b, %s FROM %s WHERE at >= ? AND at < ? GROUP BY %s, b`,
			r.key, bucket, bucket, strings.Join(r.aggs, ", "), r.table, r.key), from, until)
		if err != nil {
			return 0, fmt.Errorf("roll up %s: %w", r.table, err)
		}
		var merged [][]any
		for rows.Next() {
			vals := make([]any, len(r.cols)+2)
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return 0, err
			}
			merged = append(merged, vals)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM `+r.table+` WHERE at >= ? AND at < ?`, from, until)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		replaced += n
		insert := fmt.Sprintf(`INSERT INTO %s (%s, at, %s) VALUES (?%s)`,
			r.table, r.key, strings.Join(r.cols, ", "), strings.Repeat(", ?", len(r.cols)+1))
		for _, vals := range merged {
			if _, err := tx.ExecContext(ctx, insert, vals...); err != nil {
				return 0, fmt.Errorf("store rolled-up %s: %w", r.table, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		settingMetricsRolledUp, strconv.FormatInt(until, 10)); err != nil {
		return 0, err
	}
	return replaced, tx.Commit()
}
