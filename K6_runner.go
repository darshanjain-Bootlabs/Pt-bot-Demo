package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ===================== GLOBALS =====================

var db *sql.DB

// ===================== LOGGER & LOKI =====================

type Logger struct {
	file  *os.File
	mutex sync.Mutex
}

func NewLogger(script string) *Logger {
	os.MkdirAll("logs", os.ModePerm)

	safeName := strings.ReplaceAll(script, "/", "_")
	safeName = strings.ReplaceAll(safeName, ".js", "")

	filePath := fmt.Sprintf("logs/%s.log", safeName)

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("Error creating log file:", err)
		return nil
	}

	return &Logger{file: file}
}

func (l *Logger) Log(script string, level string, message string) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	timestamp := time.Now().Format("2006-01-02 15:04:05")

	logJSON := map[string]interface{}{
		"time":    timestamp,
		"script":  script,
		"level":   level,
		"message": message,
	}

	jsonData, _ := json.Marshal(logJSON)

	// File
	l.file.WriteString(string(jsonData) + "\n")

	// Terminal
	fmt.Println(string(jsonData))

	// Loki (Using internal Docker DNS 'loki')
	go pushLogToLoki(script, string(jsonData))
}

func pushLogToLoki(script string, logLine string) {
	url := "http://loki:3100/loki/api/v1/push"

	timestamp := fmt.Sprintf("%d", time.Now().UnixNano())

	payload := map[string]interface{}{
		"streams": []map[string]interface{}{
			{
				"stream": map[string]string{
					"job":    "k6-runner",
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

// ===================== DB =====================

func initDB() {
	// Using internal Docker DNS 'timescaledb'
	connStr := "postgres://postgres:password@timescaledb:5432/k6metrics?sslmode=disable"

	var err error
	for i := 0; i < 5; i++ {
		db, err = sql.Open("postgres", connStr)
		if err == nil {
			err = db.Ping()
			if err == nil {
				break
			}
		}
		fmt.Println("Waiting for TimescaleDB...")
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		panic(fmt.Sprintf("Failed to connect to database after retries: %v", err))
	}

	// Create test_runs table for post-run summaries
	// The metrics table is created by init.sql with the correct xk6-output-timescaledb schema
	schema := `
	CREATE TABLE IF NOT EXISTS test_runs (
		id SERIAL PRIMARY KEY,
		script_name TEXT,
		status TEXT,
		start_time TIMESTAMPTZ,
		end_time TIMESTAMPTZ,
		
		-- Request metrics
		total_requests INT,
		failed_requests INT,
		request_rate FLOAT,
		
		-- Latency metrics (milliseconds)
		avg_latency FLOAT,
		p50_latency FLOAT,
		p95_latency FLOAT,
		p99_latency FLOAT,
		min_latency FLOAT,
		max_latency FLOAT,
		
		-- Timing breakdown
		avg_blocked FLOAT,
		avg_connecting FLOAT,
		avg_tls_connecting FLOAT,
		avg_sending FLOAT,
		avg_waiting FLOAT,
		avg_receiving FLOAT,
		
		-- Data transfer
		total_data_sent BIGINT,
		total_data_received BIGINT,
		
		-- Virtual Users
		max_vus INT,
		
		-- Iterations
		total_iterations INT,
		avg_iter_duration FLOAT,
		
		-- Checks
		checks_passed INT,
		checks_failed INT,
		
		-- Errors
		total_errors INT,
		error_rate FLOAT,
		dropped_iterations INT,
		
		-- Raw summary for debugging
		raw_summary JSONB
	);`

	_, err = db.Exec(schema)
	if err != nil {
		fmt.Printf("Failed to initialize test_runs table: %v\n", err)
		os.Exit(1)
	}

	// Verify metrics table exists (created by init.sql for xk6-output-timescaledb)
	var metricsExists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='metrics')").Scan(&metricsExists)
	if err != nil || !metricsExists {
		fmt.Println("⚠️  WARNING: metrics table not found. Make sure init.sql ran correctly on TimescaleDB startup.")
	} else {
		fmt.Println("✅ metrics table ready for xk6-output-timescaledb extension.")
	}

	fmt.Println("✅ Connected to TimescaleDB and schema verified.")
	fmt.Println("📊 Real-time metrics → 'metrics' table (via xk6-output-timescaledb)")
	fmt.Println("📋 Post-run summaries → 'test_runs' table (via /internal/summary)")
}

// ===================== RUN K6 =====================

func runK6(scriptPath string, wg *sync.WaitGroup) {
	defer wg.Done()

	logger := NewLogger(scriptPath)
	if logger == nil {
		return
	}
	defer logger.file.Close()

	logger.Log(scriptPath, "INFO", "Starting execution")

	scriptContent, err := os.ReadFile(scriptPath)
	if err != nil {
		logger.Log(scriptPath, "ERROR", fmt.Sprintf("Could not read script: %v", err))
		return
	}

	// Inject handleSummary so k6 posts its end-of-run summary to our Go server.
	// This is separate from the real-time metrics the xk6-output-timescaledb extension writes.
	reportingJS := fmt.Sprintf(`
import http from "k6/http";

export function handleSummary(data) {
	data.script_name = "%s";
	http.post("http://localhost:8080/internal/summary", JSON.stringify(data), {
		headers: { "Content-Type": "application/json" },
	});
}
`, scriptPath)

	finalScript := string(scriptContent) + "\n" + reportingJS

	// Dual output:
	// 1. experimental-prometheus-rw  → Prometheus (for Grafana dashboards)
	// 2. timescaledb                 → TimescaleDB metrics table (raw time-series via xk6 extension)
	timescaleConnStr := "postgres://postgres:password@timescaledb:5432/k6metrics?sslmode=disable"

	cmd := exec.Command(
		"/app/k6", "run", "-",
		"-o", "experimental-prometheus-rw",
		"-o", fmt.Sprintf("timescaledb=%s", timescaleConnStr),
	)

	cmd.Stdin = strings.NewReader(finalScript)

	// Prometheus Remote Write endpoint
	cmd.Env = append(os.Environ(), "K6_PROMETHEUS_RW_SERVER_URL=http://prometheus:9090/api/v1/write")

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	cmd.Start()

	var goroutineWg sync.WaitGroup

	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) != "" && !strings.Contains(line, "k6.io") {
				logger.Log(scriptPath, "INFO", line)
			}
		}
	}()

	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.TrimSpace(text) != "" {
				logger.Log(scriptPath, "ERROR", text)
			}
		}
	}()

	err = cmd.Wait()
	goroutineWg.Wait()

	if err != nil {
		logger.Log(scriptPath, "ERROR", fmt.Sprintf("k6 process failed: %v", err))
	} else {
		logger.Log(scriptPath, "INFO", "Execution completed successfully")
	}

	// Small delay to ensure TimescaleDB extension finishes writing
	time.Sleep(1 * time.Second)
}

func metricsCheckHandler(w http.ResponseWriter, r *http.Request) {
	// Query how many raw metric rows exist (written by xk6-output-timescaledb)
	var metricsCount int
	err := db.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&metricsCount)
	if err != nil {
		fmt.Fprintf(w, "Error querying metrics: %v\n", err)
		return
	}

	// Query test_runs summary count
	var testRunsCount int
	err = db.QueryRow("SELECT COUNT(*) FROM test_runs").Scan(&testRunsCount)
	if err != nil {
		testRunsCount = -1
	}

	// Query unique scripts from test_runs
	rows, err := db.Query("SELECT DISTINCT script_name FROM test_runs ORDER BY script_name")
	if err != nil {
		fmt.Fprintf(w, "Error querying scripts: %v\n", err)
		return
	}
	defer rows.Close()

	var scripts []string
	for rows.Next() {
		var script string
		rows.Scan(&script)
		scripts = append(scripts, script)
	}

	// Get test_runs schema info
	schemaRows, err := db.Query(`
		SELECT column_name, data_type 
		FROM information_schema.columns 
		WHERE table_name = 'test_runs' 
		ORDER BY ordinal_position
	`)
	if err != nil {
		fmt.Fprintf(w, "Error querying schema: %v\n", err)
		return
	}
	defer schemaRows.Close()

	type Column struct {
		Name     string `json:"name"`
		DataType string `json:"data_type"`
	}
	var columns []Column
	for schemaRows.Next() {
		var col Column
		schemaRows.Scan(&col.Name, &col.DataType)
		columns = append(columns, col)
	}

	response := map[string]interface{}{
		"time_series_metrics_rows": metricsCount,  // raw rows from xk6 extension
		"test_runs_count":          testRunsCount, // one row per completed run
		"scripts":                  scripts,
		"test_runs_schema":         columns,
		"status":                   "success",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func metricsDetailHandler(w http.ResponseWriter, r *http.Request) {
	// Get recent test runs with comprehensive metrics
	rows, err := db.Query(`
		SELECT 
			script_name, status, start_time, end_time,
			total_requests, failed_requests, error_rate,
			avg_latency, p95_latency, p99_latency, max_latency,
			total_data_sent, total_data_received,
			max_vus, total_iterations,
			checks_passed, checks_failed,
			total_errors
		FROM test_runs 
		ORDER BY start_time DESC
		LIMIT 50
	`)
	if err != nil {
		fmt.Fprintf(w, "Error querying metrics: %v\n", err)
		return
	}
	defer rows.Close()

	type TestRunDetail struct {
		ScriptName      string      `json:"script_name"`
		Status          string      `json:"status"`
		StartTime       interface{} `json:"start_time"`
		EndTime         interface{} `json:"end_time"`
		TotalRequests   int         `json:"total_requests"`
		FailedRequests  int         `json:"failed_requests"`
		ErrorRate       float64     `json:"error_rate"`
		AvgLatency      float64     `json:"avg_latency_ms"`
		P95Latency      float64     `json:"p95_latency_ms"`
		P99Latency      float64     `json:"p99_latency_ms"`
		MaxLatency      float64     `json:"max_latency_ms"`
		DataSent        int64       `json:"data_sent_bytes"`
		DataReceived    int64       `json:"data_received_bytes"`
		MaxVUs          int         `json:"max_vus"`
		TotalIterations int         `json:"total_iterations"`
		ChecksPassed    int         `json:"checks_passed"`
		ChecksFailed    int         `json:"checks_failed"`
		TotalErrors     int         `json:"total_errors"`
	}

	var details []TestRunDetail
	for rows.Next() {
		var detail TestRunDetail
		err := rows.Scan(
			&detail.ScriptName, &detail.Status, &detail.StartTime, &detail.EndTime,
			&detail.TotalRequests, &detail.FailedRequests, &detail.ErrorRate,
			&detail.AvgLatency, &detail.P95Latency, &detail.P99Latency, &detail.MaxLatency,
			&detail.DataSent, &detail.DataReceived,
			&detail.MaxVUs, &detail.TotalIterations,
			&detail.ChecksPassed, &detail.ChecksFailed,
			&detail.TotalErrors,
		)
		if err != nil {
			fmt.Printf("Error scanning row: %v\n", err)
			continue
		}
		details = append(details, detail)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(details)
}

// ===================== METRICS PARSER =====================

type K6Summary struct {
	Metrics map[string]struct {
		Values map[string]interface{} `json:"values"`
		Type   string                 `json:"type"`
	} `json:"metrics"`
	State map[string]interface{} `json:"state"`
}

func extractFloat(values map[string]interface{}, key string) float64 {
	if v, exists := values[key]; exists {
		switch val := v.(type) {
		case float64:
			return val
		case int:
			return float64(val)
		}
	}
	return 0
}

func extractInt(values map[string]interface{}, key string) int {
	if v, exists := values[key]; exists {
		switch val := v.(type) {
		case float64:
			return int(val)
		case int:
			return val
		}
	}
	return 0
}

func parseK6Summary(bodyBytes []byte) map[string]interface{} {
	var summary K6Summary
	json.Unmarshal(bodyBytes, &summary)

	metrics := make(map[string]interface{})

	// Request metrics
	if httpReqs, exists := summary.Metrics["http_reqs"]; exists {
		metrics["total_requests"] = extractInt(httpReqs.Values, "value")
	}

	if httpReqFailed, exists := summary.Metrics["http_req_failed"]; exists {
		metrics["failed_requests"] = extractInt(httpReqFailed.Values, "value")
	}

	// Latency metrics (already in milliseconds from k6)
	if httpReqDuration, exists := summary.Metrics["http_req_duration"]; exists {
		metrics["avg_latency"] = extractFloat(httpReqDuration.Values, "avg")
		metrics["p50_latency"] = extractFloat(httpReqDuration.Values, "p(50)")
		metrics["p95_latency"] = extractFloat(httpReqDuration.Values, "p(95)")
		metrics["p99_latency"] = extractFloat(httpReqDuration.Values, "p(99)")
		metrics["min_latency"] = extractFloat(httpReqDuration.Values, "min")
		metrics["max_latency"] = extractFloat(httpReqDuration.Values, "max")
	}

	// Timing breakdown
	if blocked, exists := summary.Metrics["http_req_blocked"]; exists {
		metrics["avg_blocked"] = extractFloat(blocked.Values, "avg")
	}
	if connecting, exists := summary.Metrics["http_req_connecting"]; exists {
		metrics["avg_connecting"] = extractFloat(connecting.Values, "avg")
	}
	if tlsConnecting, exists := summary.Metrics["http_req_tls_connecting"]; exists {
		metrics["avg_tls_connecting"] = extractFloat(tlsConnecting.Values, "avg")
	}
	if sending, exists := summary.Metrics["http_req_sending"]; exists {
		metrics["avg_sending"] = extractFloat(sending.Values, "avg")
	}
	if waiting, exists := summary.Metrics["http_req_waiting"]; exists {
		metrics["avg_waiting"] = extractFloat(waiting.Values, "avg")
	}
	if receiving, exists := summary.Metrics["http_req_receiving"]; exists {
		metrics["avg_receiving"] = extractFloat(receiving.Values, "avg")
	}

	// Data metrics
	if dataSent, exists := summary.Metrics["data_sent"]; exists {
		metrics["total_data_sent"] = extractInt(dataSent.Values, "value")
	}
	if dataReceived, exists := summary.Metrics["data_received"]; exists {
		metrics["total_data_received"] = extractInt(dataReceived.Values, "value")
	}

	// VUs
	if vusMax, exists := summary.Metrics["vus_max"]; exists {
		metrics["max_vus"] = extractInt(vusMax.Values, "value")
	}

	// Iterations
	if iterations, exists := summary.Metrics["iterations"]; exists {
		metrics["total_iterations"] = extractInt(iterations.Values, "value")
	}
	if iterDuration, exists := summary.Metrics["iteration_duration"]; exists {
		metrics["avg_iter_duration"] = extractFloat(iterDuration.Values, "avg")
	}

	// Checks
	if checks, exists := summary.Metrics["checks"]; exists {
		metrics["checks_passed"] = extractInt(checks.Values, "passes")
		metrics["checks_failed"] = extractInt(checks.Values, "fails")
	}

	// Errors
	if errors, exists := summary.Metrics["errors"]; exists {
		metrics["total_errors"] = extractInt(errors.Values, "value")
	}

	if droppedIterations, exists := summary.Metrics["dropped_iterations"]; exists {
		metrics["dropped_iterations"] = extractInt(droppedIterations.Values, "value")
	}

	// Calculate error rate
	if totalReqs, ok := metrics["total_requests"].(int); ok && totalReqs > 0 {
		if failed, ok := metrics["failed_requests"].(int); ok {
			metrics["error_rate"] = float64(failed) / float64(totalReqs) * 100
		}
	}

	// Calculate request rate
	if totalReqs, ok := metrics["total_requests"].(int); ok {
		metrics["request_rate"] = float64(totalReqs) / 10.0 // Assumes 10s test duration
	}

	return metrics
}

// ===================== API =====================

func runHandler(w http.ResponseWriter, r *http.Request) {
	var scripts []string

	err := json.NewDecoder(r.Body).Decode(&scripts)
	if err != nil {
		http.Error(w, "Invalid request", 400)
		return
	}

	var wg sync.WaitGroup

	for _, script := range scripts {
		wg.Add(1)
		go runK6(script, &wg)
	}

	go func() {
		wg.Wait()
		fmt.Println("🎯 All scripts completed")
	}()

	w.Write([]byte("✅ Execution started"))
}

func summaryHandler(w http.ResponseWriter, r *http.Request) {
	var rawData map[string]interface{}
	bodyBytes, _ := io.ReadAll(r.Body)

	json.Unmarshal(bodyBytes, &rawData)

	// Extract script name
	scriptName := ""
	if sn, exists := rawData["script_name"]; exists {
		scriptName = sn.(string)
	}

	// Parse all metrics from k6 summary
	parsedMetrics := parseK6Summary(bodyBytes)

	insertQuery := `
		INSERT INTO test_runs (
			script_name, status, start_time, end_time,
			total_requests, failed_requests, request_rate,
			avg_latency, p50_latency, p95_latency, p99_latency, min_latency, max_latency,
			avg_blocked, avg_connecting, avg_tls_connecting, avg_sending, avg_waiting, avg_receiving,
			total_data_sent, total_data_received,
			max_vus,
			total_iterations, avg_iter_duration,
			checks_passed, checks_failed,
			total_errors, error_rate, dropped_iterations,
			raw_summary
		) VALUES (
			$1, $2, NOW() - INTERVAL '10 minutes', NOW(),
			$3, $4, $5,
			$6, $7, $8, $9, $10, $11,
			$12, $13, $14, $15, $16, $17,
			$18, $19,
			$20,
			$21, $22,
			$23, $24,
			$25, $26, $27,
			$28
		)
	`

	_, err := db.Exec(insertQuery,
		scriptName, "COMPLETED",
		parsedMetrics["total_requests"],
		parsedMetrics["failed_requests"],
		parsedMetrics["request_rate"],
		parsedMetrics["avg_latency"],
		parsedMetrics["p50_latency"],
		parsedMetrics["p95_latency"],
		parsedMetrics["p99_latency"],
		parsedMetrics["min_latency"],
		parsedMetrics["max_latency"],
		parsedMetrics["avg_blocked"],
		parsedMetrics["avg_connecting"],
		parsedMetrics["avg_tls_connecting"],
		parsedMetrics["avg_sending"],
		parsedMetrics["avg_waiting"],
		parsedMetrics["avg_receiving"],
		parsedMetrics["total_data_sent"],
		parsedMetrics["total_data_received"],
		parsedMetrics["max_vus"],
		parsedMetrics["total_iterations"],
		parsedMetrics["avg_iter_duration"],
		parsedMetrics["checks_passed"],
		parsedMetrics["checks_failed"],
		parsedMetrics["total_errors"],
		parsedMetrics["error_rate"],
		parsedMetrics["dropped_iterations"],
		bodyBytes,
	)

	if err != nil {
		fmt.Printf("❌ Error saving summary to DB: %v\n", err)
		fmt.Printf("   Script: %s\n", scriptName)
		http.Error(w, fmt.Sprintf("Error saving summary: %v", err), 500)
		return
	}

	fmt.Printf("✅ Summary saved for: %s\n", scriptName)
	fmt.Printf("   Requests: %d | P95: %.2fms | Data Sent: %v bytes | Errors: %d\n",
		parsedMetrics["total_requests"],
		parsedMetrics["p95_latency"],
		parsedMetrics["total_data_sent"],
		parsedMetrics["total_errors"],
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Metrics stored"})
}

// ===================== MAIN =====================

func main() {
	initDB()

	go func() {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		fmt.Println("📊 Prometheus metrics exposed on :2112")

		if err := http.ListenAndServe(":2112", metricsMux); err != nil {
			fmt.Printf("Metrics server error: %v\n", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/run", runHandler)
	mux.HandleFunc("/internal/summary", summaryHandler)
	mux.HandleFunc("/metrics-check", metricsCheckHandler)
	mux.HandleFunc("/metrics-detail", metricsDetailHandler)

	fmt.Println("🚀 Runner Agent API running on :8080")
	fmt.Println("➡️  Trigger tests via: POST /run")

	http.ListenAndServe(":8080", mux)
}
