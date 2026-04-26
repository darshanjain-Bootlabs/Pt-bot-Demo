package main

import (
	"bufio"
	"bytes"
	"context"
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
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// ===================== GLOBALS =====================

var db *sql.DB
var temporalClient client.Client

// ===================== DATA STRUCTS =====================

// LoadTestInput defines the data passed into the workflow
type LoadTestInput struct {
	ScriptPath string
	TestRunID  string
}

// MetricsData holds the aggregated results fetched from TimescaleDB
type MetricsData struct {
	TestRunID  string
	ScriptName string
	TotalReqs  int
	FailedReqs int
	P95Latency float64
	MaxVUs     int
}

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

func (l *Logger) Close() {
	l.file.Close()
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

	schema := `
    CREATE TABLE IF NOT EXISTS test_runs (
        id SERIAL PRIMARY KEY,
        test_run_id TEXT,
        script_name TEXT,
        status TEXT,
        start_time TIMESTAMPTZ,
        end_time TIMESTAMPTZ,
        
        total_requests INT,
        failed_requests INT,
        request_rate FLOAT,
        
        avg_latency FLOAT,
        p50_latency FLOAT,
        p95_latency FLOAT,
        p99_latency FLOAT,
        min_latency FLOAT,
        max_latency FLOAT,
        
        avg_blocked FLOAT,
        avg_connecting FLOAT,
        avg_tls_connecting FLOAT,
        avg_sending FLOAT,
        avg_waiting FLOAT,
        avg_receiving FLOAT,
        
        total_data_sent BIGINT,
        total_data_received BIGINT,
        
        max_vus INT,
        total_iterations INT,
        avg_iter_duration FLOAT,
        
        checks_passed INT,
        checks_failed INT,
        
        total_errors INT,
        error_rate FLOAT,
        dropped_iterations INT,
        
        raw_summary JSONB
    );`

	_, err = db.Exec(schema)
	if err != nil {
		fmt.Printf("Failed to initialize test_runs table: %v\n", err)
		os.Exit(1)
	}

	// Gentle migration if the table existed before we added test_run_id
	_, _ = db.Exec("ALTER TABLE test_runs ADD COLUMN IF NOT EXISTS test_run_id TEXT;")

	var metricsExists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='metrics')").Scan(&metricsExists)
	if err != nil || !metricsExists {
		fmt.Println("⚠️  WARNING: metrics table not found. Make sure init.sql ran correctly on TimescaleDB startup.")
	} else {
		fmt.Println("✅ metrics table ready for xk6-output-timescaledb extension.")
	}

	fmt.Println("✅ Connected to TimescaleDB and schema verified.")
}

// ===================== TEMPORAL ACTIVITIES =====================

// Activity 1: Execute the k6 Load Test
func ExecuteK6Activity(ctx context.Context, input LoadTestInput) error {
	scriptPath := input.ScriptPath
	testRunID := input.TestRunID

	logger := NewLogger(scriptPath)
	if logger == nil {
		return fmt.Errorf("could not create logger for %s", scriptPath)
	}
	defer logger.Close()

	logger.Log(scriptPath, "INFO", fmt.Sprintf("Starting Temporal execution with test_run_id: %s", testRunID))

	scriptContent, err := os.ReadFile(scriptPath)
	if err != nil {
		logger.Log(scriptPath, "ERROR", fmt.Sprintf("Could not read script: %v", err))
		return err
	}

	reportingJS := fmt.Sprintf(`
import __internal_http from "k6/http";

export function handleSummary(data) {
    data.script_name = "%s";
    data.test_run_id = "%s";
    __internal_http.post("http://localhost:8080/internal/summary", JSON.stringify(data), {
        headers: { "Content-Type": "application/json" },
    });
}
`, scriptPath, testRunID)

	finalScript := string(scriptContent) + "\n" + reportingJS

	timescaleConnStr := "postgresql://postgres:password@timescaledb:5432/k6metrics?sslmode=disable"

	// Bypassing Temporal for metrics: data flows directly to TimescaleDB
	cmd := exec.CommandContext(
		ctx,
		"/app/k6", "run", "-",
		"--tag", fmt.Sprintf("test_run_id=%s", testRunID),
		"-o", "experimental-prometheus-rw", fmt.Sprintf("timescaledb=%s", timescaleConnStr),
	)

	cmd.Stdin = strings.NewReader(finalScript)
	cmd.Env = append(os.Environ(), "K6_PROMETHEUS_RW_SERVER_URL=http://prometheus:9090/api/v1/write")

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start k6: %w", err)
	}

	var scanWg sync.WaitGroup

	scanWg.Add(1)
	go func() {
		defer scanWg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) != "" && !strings.Contains(line, "k6.io") {
				logger.Log(scriptPath, "INFO", line)
			}
		}
	}()

	scanWg.Add(1)
	go func() {
		defer scanWg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.TrimSpace(text) != "" {
				logger.Log(scriptPath, "ERROR", text)
			}
		}
	}()

	scanWg.Wait()
	err = cmd.Wait()

	if err != nil {
		logger.Log(scriptPath, "ERROR", fmt.Sprintf("k6 process failed: %v", err))
		return err
	}

	logger.Log(scriptPath, "INFO", "Execution completed successfully")
	time.Sleep(1 * time.Second)
	return nil
}

// Activity 2: Fetch Database Metrics
func FetchDatabaseMetricsActivity(ctx context.Context, testRunID string) (MetricsData, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("Fetching metrics from TimescaleDB", "TestRunID", testRunID)

	// Buffer to allow the k6 handleSummary webhook to finish writing the row to TimescaleDB
	time.Sleep(5 * time.Second)

	var data MetricsData
	query := `
		SELECT test_run_id, script_name, total_requests, failed_requests, p95_latency, max_vus
		FROM test_runs
		WHERE test_run_id = $1
	`

	err := db.QueryRowContext(ctx, query, testRunID).Scan(
		&data.TestRunID, &data.ScriptName, &data.TotalReqs,
		&data.FailedReqs, &data.P95Latency, &data.MaxVUs,
	)

	if err != nil {
		return data, fmt.Errorf("failed to fetch metrics for %s: %v", testRunID, err)
	}

	return data, nil
}

// Activity 3: Generate AI Summary
func GenerateAISummaryActivity(ctx context.Context, data MetricsData) (string, error) {
	prompt := fmt.Sprintf(`
		Analyze this load test result for script '%s':
		- Total Requests: %d
		- Failed Requests: %d
		- P95 Latency: %.2fms
		- Peak VUs: %d
	`, data.ScriptName, data.TotalReqs, data.FailedReqs, data.P95Latency, data.MaxVUs)

	activity.GetLogger(ctx).Info("Sending prompt to AI API...", "Prompt", prompt)

	// Mocking AI Evaluation
	status := "PASSED"
	if data.FailedReqs > 0 || data.P95Latency > 500.0 {
		status = "FAILED"
	}

	summary := fmt.Sprintf(
		"[%s] The test for %s reached %d peak users. P95 latency was %.2fms with %d errors.",
		status, data.ScriptName, data.MaxVUs, data.P95Latency, data.FailedReqs,
	)

	return summary, nil
}

// Activity 4: Send Slack Alert
func SendSlackAlertActivity(ctx context.Context, summary string) error {
	fmt.Println("\n==================================================")
	fmt.Printf("🔔 [SLACK ALERT TRIGGERED]\n%s\n", summary)
	fmt.Println("==================================================\n")
	return nil
}

// ===================== TEMPORAL WORKFLOW =====================

func LoadTestWorkflow(ctx workflow.Context, input LoadTestInput) (string, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Hour,
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	logger := workflow.GetLogger(ctx)

	logger.Info("Starting LoadTestWorkflow", "TestRunID", input.TestRunID, "Script", input.ScriptPath)

	// ACTIVITY 1: Execute the k6 Load Test
	err := workflow.ExecuteActivity(ctx, ExecuteK6Activity, input).Get(ctx, nil)
	if err != nil {
		logger.Error("k6 Execution Activity failed", "Error", err)
		return "", err
	}

	// ACTIVITY 2: Fetch the resulting metrics from TimescaleDB
	dbCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
	})

	var metricsData MetricsData
	err = workflow.ExecuteActivity(dbCtx, FetchDatabaseMetricsActivity, input.TestRunID).Get(ctx, &metricsData)
	if err != nil {
		logger.Error("Failed to fetch metrics from DB", "Error", err)
		return "", err
	}

	// ACTIVITY 3: Generate the AI Summary
	apiCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
	})

	var summary string
	err = workflow.ExecuteActivity(apiCtx, GenerateAISummaryActivity, metricsData).Get(ctx, &summary)
	if err != nil {
		logger.Error("Failed to generate AI summary", "Error", err)
		return "", err
	}

	// ACTIVITY 4: Send the Slack Alert
	err = workflow.ExecuteActivity(apiCtx, SendSlackAlertActivity, summary).Get(ctx, nil)
	if err != nil {
		logger.Error("Failed to send Slack Alert", "Error", err)
		return "", err
	}

	logger.Info("LoadTestWorkflow completed successfully", "TestRunID", input.TestRunID)
	return "Workflow completed successfully", nil
}

// ===================== API =====================

func runHandler(w http.ResponseWriter, r *http.Request) {
	var scripts []string

	err := json.NewDecoder(r.Body).Decode(&scripts)
	if err != nil {
		http.Error(w, "Invalid request", 400)
		return
	}

	queuedRuns := make([]map[string]string, 0, len(scripts))

	for i, script := range scripts {
		testRunID := fmt.Sprintf("tr_%d_%d", time.Now().UnixNano(), i)
		workflowID := fmt.Sprintf("loadtest-%s", testRunID)

		options := client.StartWorkflowOptions{
			ID:        workflowID,
			TaskQueue: "LOAD_TEST_TASK_QUEUE",
		}

		input := LoadTestInput{
			ScriptPath: script,
			TestRunID:  testRunID,
		}

		// Dispatch to Temporal Orchestrator
		we, err := temporalClient.ExecuteWorkflow(context.Background(), options, LoadTestWorkflow, input)
		if err != nil {
			fmt.Printf("❌ Failed to start Temporal workflow for %s: %v\n", script, err)
			continue
		}

		queuedRuns = append(queuedRuns, map[string]string{
			"script":      script,
			"test_run_id": testRunID,
			"workflow_id": we.GetID(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "orchestration_started",
		"message": "Workflows dispatched to Temporal successfully",
		"runs":    queuedRuns,
	})
}

func summaryHandler(w http.ResponseWriter, r *http.Request) {
	var rawData map[string]interface{}
	bodyBytes, _ := io.ReadAll(r.Body)

	json.Unmarshal(bodyBytes, &rawData)

	scriptName := ""
	if sn, exists := rawData["script_name"]; exists {
		scriptName = sn.(string)
	}

	testRunID := ""
	if tr, exists := rawData["test_run_id"]; exists {
		testRunID = tr.(string)
	}

	parsedMetrics := parseK6Summary(bodyBytes)

	insertQuery := `
        INSERT INTO test_runs (
            test_run_id, script_name, status, start_time, end_time,
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
            $1, $2, $3, NOW() - INTERVAL '10 minutes', NOW(),
            $4, $5, $6,
            $7, $8, $9, $10, $11, $12,
            $13, $14, $15, $16, $17, $18,
            $19, $20,
            $21,
            $22, $23,
            $24, $25,
            $26, $27, $28,
            $29
        )
    `

	_, err := db.Exec(insertQuery,
		testRunID, scriptName, "COMPLETED",
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
		http.Error(w, fmt.Sprintf("Error saving summary: %v", err), 500)
		return
	}

	fmt.Printf("✅ Summary saved for: %s (ID: %s)\n", scriptName, testRunID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Metrics stored"})
}

func metricsCheckHandler(w http.ResponseWriter, r *http.Request) {
	var metricsCount int
	err := db.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&metricsCount)
	if err != nil {
		fmt.Fprintf(w, "Error querying metrics: %v\n", err)
		return
	}

	var testRunsCount int
	err = db.QueryRow("SELECT COUNT(*) FROM test_runs").Scan(&testRunsCount)
	if err != nil {
		testRunsCount = -1
	}

	response := map[string]interface{}{
		"time_series_metrics_rows": metricsCount,
		"test_runs_count":          testRunsCount,
		"status":                   "success",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func metricsDetailHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
        SELECT 
            test_run_id, script_name, status, start_time, end_time,
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
		TestRunID       string      `json:"test_run_id"`
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
			&detail.TestRunID, &detail.ScriptName, &detail.Status, &detail.StartTime, &detail.EndTime,
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

	if httpReqs, exists := summary.Metrics["http_reqs"]; exists {
		metrics["total_requests"] = extractInt(httpReqs.Values, "value")
	}
	if httpReqFailed, exists := summary.Metrics["http_req_failed"]; exists {
		metrics["failed_requests"] = extractInt(httpReqFailed.Values, "value")
	}
	if httpReqDuration, exists := summary.Metrics["http_req_duration"]; exists {
		metrics["avg_latency"] = extractFloat(httpReqDuration.Values, "avg")
		metrics["p50_latency"] = extractFloat(httpReqDuration.Values, "p(50)")
		metrics["p95_latency"] = extractFloat(httpReqDuration.Values, "p(95)")
		metrics["p99_latency"] = extractFloat(httpReqDuration.Values, "p(99)")
		metrics["min_latency"] = extractFloat(httpReqDuration.Values, "min")
		metrics["max_latency"] = extractFloat(httpReqDuration.Values, "max")
	}
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
	if dataSent, exists := summary.Metrics["data_sent"]; exists {
		metrics["total_data_sent"] = extractInt(dataSent.Values, "value")
	}
	if dataReceived, exists := summary.Metrics["data_received"]; exists {
		metrics["total_data_received"] = extractInt(dataReceived.Values, "value")
	}
	if vusMax, exists := summary.Metrics["vus_max"]; exists {
		metrics["max_vus"] = extractInt(vusMax.Values, "value")
	}
	if iterations, exists := summary.Metrics["iterations"]; exists {
		metrics["total_iterations"] = extractInt(iterations.Values, "value")
	}
	if iterDuration, exists := summary.Metrics["iteration_duration"]; exists {
		metrics["avg_iter_duration"] = extractFloat(iterDuration.Values, "avg")
	}
	if checks, exists := summary.Metrics["checks"]; exists {
		metrics["checks_passed"] = extractInt(checks.Values, "passes")
		metrics["checks_failed"] = extractInt(checks.Values, "fails")
	}
	if errors, exists := summary.Metrics["errors"]; exists {
		metrics["total_errors"] = extractInt(errors.Values, "value")
	}
	if droppedIterations, exists := summary.Metrics["dropped_iterations"]; exists {
		metrics["dropped_iterations"] = extractInt(droppedIterations.Values, "value")
	}

	if totalReqs, ok := metrics["total_requests"].(int); ok && totalReqs > 0 {
		if failed, ok := metrics["failed_requests"].(int); ok {
			metrics["error_rate"] = float64(failed) / float64(totalReqs) * 100
		}
	}
	if totalReqs, ok := metrics["total_requests"].(int); ok {
		metrics["request_rate"] = float64(totalReqs) / 10.0
	}

	return metrics
}

// ===================== MAIN =====================

func main() {
	initDB()

	// 1. Initialize the Temporal Client
	var err error
	temporalClient, err = client.Dial(client.Options{})
	if err != nil {
		fmt.Printf("❌ Failed to create Temporal client: %v\n", err)
		os.Exit(1)
	}
	defer temporalClient.Close()
	fmt.Println("✅ Connected to Temporal Server")

	// 2. Initialize the Temporal Worker
	testWorker := worker.New(temporalClient, "LOAD_TEST_TASK_QUEUE", worker.Options{})

	// 3. Register our Workflow and Activities with the Worker
	testWorker.RegisterWorkflow(LoadTestWorkflow)
	testWorker.RegisterActivity(ExecuteK6Activity)
	testWorker.RegisterActivity(FetchDatabaseMetricsActivity)
	testWorker.RegisterActivity(GenerateAISummaryActivity)
	testWorker.RegisterActivity(SendSlackAlertActivity)

	// 4. Start the Worker in a goroutine
	go func() {
		fmt.Println("👷 Temporal Worker started, listening to LOAD_TEST_TASK_QUEUE")
		err := testWorker.Start()
		if err != nil {
			fmt.Printf("❌ Failed to start Temporal worker: %v\n", err)
		}
	}()

	// 5. Start Prometheus Metrics Server
	go func() {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		fmt.Println("📊 Prometheus metrics exposed on :2112")

		if err := http.ListenAndServe(":2112", metricsMux); err != nil {
			fmt.Printf("Metrics server error: %v\n", err)
		}
	}()

	// 6. Start the Main API Server
	mux := http.NewServeMux()
	mux.HandleFunc("/run", runHandler)
	mux.HandleFunc("/internal/summary", summaryHandler)
	mux.HandleFunc("/metrics-check", metricsCheckHandler)
	mux.HandleFunc("/metrics-detail", metricsDetailHandler)

	fmt.Println("🚀 Runner Agent API running on :8080")
	fmt.Println("➡️  Trigger tests via: POST /run")

	if err := http.ListenAndServe(":8080", mux); err != nil {
		fmt.Printf("Main server error: %v\n", err)
	}
}
