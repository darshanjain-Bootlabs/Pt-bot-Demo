package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ===================== GLOBALS =====================

var db *sql.DB

// ===================== METRICS =====================

var (
	requestsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k6_requests_total",
			Help: "Total requests per script",
		},
		[]string{"script"},
	)

	latencyGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k6_latency_avg_ms",
			Help: "Average latency per script",
		},
		[]string{"script"},
	)

	p95Gauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k6_latency_p95_ms",
			Help: "P95 latency per script",
		},
		[]string{"script"},
	)
)

func init() {
	prometheus.MustRegister(requestsGauge)
	prometheus.MustRegister(latencyGauge)
	prometheus.MustRegister(p95Gauge)
}

// ===================== METRICS STRUCT =====================

type Metrics struct {
	Requests   int
	AvgLatency float64
	P95Latency float64
}

// ===================== LOKI =====================

func pushLogToLoki(script string, logLine string) {
	url := "http://loki:3100/loki/api/v1/push"

	timestamp := strconv.FormatInt(time.Now().UnixNano(), 10)

	payload := map[string]interface{}{
		"streams": []map[string]interface{}{
			{
				"stream": map[string]string{
					"job":    "k6",
					"script": script,
				},
				"values": [][]string{
					{timestamp, logLine},
				},
			},
		},
	}

	jsonData, _ := json.Marshal(payload)

	http.Post(url, "application/json", bytes.NewBuffer(jsonData))
}

// ===================== PARSER =====================

func parseMetrics(line string, m *Metrics) {

	// Parse total requests
	if strings.Contains(line, "http_reqs") {
		re := regexp.MustCompile(`http_reqs.*:\s+(\d+)`)
		match := re.FindStringSubmatch(line)
		if len(match) > 1 {
			val, _ := strconv.Atoi(match[1])
			m.Requests = val
		}
	}

	// Parse latency - ENHANCED DEBUG
	if strings.Contains(line, "http_req_duration") {
		fmt.Println("🔍 FOUND http_req_duration line:", line)

		// Extract avg
		avgRe := regexp.MustCompile(`avg=([\d\.]+)([a-z]+)`)
		avgMatch := avgRe.FindStringSubmatch(line)

		if len(avgMatch) > 1 {
			avg, _ := strconv.ParseFloat(avgMatch[1], 64)
			unit := avgMatch[2]
			// Convert to ms if needed
			if unit == "s" {
				avg *= 1000
			}
			m.AvgLatency = avg
			fmt.Println("✓ Parsed avg:", avg, "unit:", unit)
		}

		// Extract p(95) - Handles both format variations
		// Match: p(95)=value followed by ms or s
		p95Re := regexp.MustCompile(`p\(95\)\s*=\s*([\d\.]+)([a-z]+)`)
		p95Match := p95Re.FindStringSubmatch(line)

		if len(p95Match) > 1 {
			p95, _ := strconv.ParseFloat(p95Match[1], 64)
			unit := p95Match[2]
			// Convert to ms if needed
			if unit == "s" {
				p95 *= 1000
			}
			m.P95Latency = p95
			fmt.Println("✓ Parsed p95:", p95, "unit:", unit)
		} else {
			fmt.Println("✗ p95 regex did NOT match")
		}

		fmt.Println("DEBUG: Final metrics -> Avg:", m.AvgLatency, "P95:", m.P95Latency)
	}
}

// ===================== DB =====================

func initDB() {
	connStr := "postgres://postgres:password@timescaledb:5432/k6metrics?sslmode=disable"

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		panic(err)
	}

	fmt.Println("✅ Connected to TimescaleDB")
}

func storeMetrics(script string, m *Metrics) {

	query := `
	INSERT INTO metrics (time, script, requests, avg_latency, p95_latency)
	VALUES (NOW(), $1, $2, $3, $4)
	`

	_, err := db.Exec(query, script, m.Requests, m.AvgLatency, m.P95Latency)
	if err != nil {
		fmt.Println("DB insert error:", err)
	}
}

// ===================== RUN K6 =====================

func runK6(script string, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("🚀 Starting %s...\n", script)

	cmd := exec.Command("k6", "run", script)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	cmd.Start()

	metrics := &Metrics{}

	// WaitGroup for goroutines to ensure they finish before we continue
	var goroutineWg sync.WaitGroup

	// STDOUT
	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()

			fmt.Printf("[%s] %s\n", script, line)
			pushLogToLoki(script, line)

			parseMetrics(line, metrics)
		}
	}()

	// STDERR
	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			errLine := scanner.Text()

			fmt.Printf("[%s ERROR] %s\n", script, errLine)
			pushLogToLoki(script, errLine)
		}
	}()

	cmd.Wait()

	// Wait for goroutines to finish reading output
	goroutineWg.Wait()

	// Prometheus
	requestsGauge.WithLabelValues(script).Set(float64(metrics.Requests))
	latencyGauge.WithLabelValues(script).Set(metrics.AvgLatency)
	p95Gauge.WithLabelValues(script).Set(metrics.P95Latency)

	// TimescaleDB
	storeMetrics(script, metrics)

	fmt.Printf("✅ Finished %s\n", script)
}

// ===================== API =====================

func runHandler(w http.ResponseWriter, r *http.Request) {
	var scripts []string

	err := json.NewDecoder(r.Body).Decode(&scripts)
	if err != nil {
		fmt.Println("❌ JSON decode error:", err)
		http.Error(w, "Invalid request", 400)
		return
	}

	fmt.Printf("🚀 Received request to run scripts: %v\n", scripts)

	var wg sync.WaitGroup

	for _, script := range scripts {
		fmt.Printf("📝 Adding script to queue: %s\n", script)
		wg.Add(1)
		go runK6(script, &wg)
	}

	go func() {
		wg.Wait()
		fmt.Println("🎯 API-triggered run completed")
	}()

	w.Write([]byte("✅ Execution started"))
}

// ===================== MAIN =====================

func main() {

	// Init DB
	initDB()

	// Metrics server (Prometheus)
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		fmt.Println("📡 Metrics running on :2112")
		http.ListenAndServe(":2112", mux)
	}()

	// API server
	mux := http.NewServeMux()
	mux.HandleFunc("/run", runHandler)

	fmt.Println("🚀 API running on :8080")
	fmt.Println("➡️ Trigger: POST /run")

	http.ListenAndServe(":8080", mux)
}
