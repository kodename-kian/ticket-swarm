package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Primary audit query
const query = `
SELECT t.id, COALESCE(c.n, 0) - t.total_stock
FROM tiers t
LEFT JOIN (
  SELECT tier_id, count(*) AS n
  FROM holds
  WHERE state IN ('active','converted')
  GROUP BY tier_id
) c ON c.tier_id = t.id`

// Defining Prometheus metrics
var (
	oversell = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "auditor_oversell", Help: "committed - total_stock; >0 is a violation"}, []string{"tier_id"})
	oversellPeak = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "auditor_oversell_peak", Help: "max oversell observed since start"}, []string{"tier_id"})
	queryDur = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "auditor_query_seconds", Help: "duration of last audit query"})
	lastOK = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "auditor_last_success_timestamp", Help: "unix time of last successful audit"})
)

// Audit function
func audit(ctx context.Context, pool *pgxpool.Pool, peaks map[string]float64) error {
	start := time.Now()

	// Run query
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Process results per tier_id
	for rows.Next() {
		var id int
		var o int64
		if err := rows.Scan(&id, &o); err != nil {
			return err
		}

		// Update raw oversell metric
		k := strconv.Itoa(id)  // Prometheus labels use strings
		oversell.WithLabelValues(k).Set(float64(o))  // Prometheus values use floats

		// Update max oversell metric
		if p, ok := peaks[k]; !ok || float64(o) > p {
			peaks[k] = float64(o)
			oversellPeak.WithLabelValues(k).Set(float64(o))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	queryDur.Set(time.Since(start).Seconds())
	lastOK.SetToCurrentTime()
	return nil
}

func main() {
	// Get data source name
	dsn := os.Getenv("AUDITOR_DSN")
	if dsn == "" {
		log.Fatal("env var AUDITOR_DSN is required")
	}

	prometheus.MustRegister(oversell, oversellPeak, queryDur, lastOK)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	peaks := map[string]float64{}  // Only modified by the loop goroutine
	go func() {
		for range time.Tick(time.Second) {
			// Timeout so a stuck query can't stall the loop silently.
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := audit(qctx, pool, peaks); err != nil {
				log.Println("audit:", err)
			}
			cancel()
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(":9100", nil))
}
