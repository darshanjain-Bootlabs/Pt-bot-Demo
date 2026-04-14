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
	// Clean the script path to create a safe file name
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

	// Loki
	go pushLogToLoki(script, string(jsonData))
}

func pushLogToLoki(script string, logLine string) {
	url := "http://localhost:3100/loki/api/v1/push" // Update to your Loki URL

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
	// Update with your TimescaleDB credentials
	connStr := "postgres://k6:k6password@localhost:5432/k6?sslmode=disable"

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		panic(err)
	}

	// Create table for final summaries
	schema := `
	CREATE TABLE IF NOT EXISTS test_runs (
		id SERIAL PRIMARY KEY,
		script_name TEXT,
		status TEXT,
		avg_latency FLOAT,
		p95_latency FLOAT,
		start_time TIMESTAMPTZ,
		end_time TIMESTAMPTZ,
		raw_summary JSONB
	);`

	_, err = db.Exec(schema)
	if err != nil {
		fmt.Printf("Failed to initialize database schema: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ Connected to TimescaleDB and schema verified.")
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

	// 1. Read the original script file
	scriptContent, err := os.ReadFile(scriptPath)
	if err != nil {
		logger.Log(scriptPath, "ERROR", fmt.Sprintf("Could not read script: %v", err))
		return
	}

	// 2. Inject the Javascript Hook
	// This captures the final summary JSON and POSTs it back to our Go server
	reportingJS := fmt.Sprintf(`
	export function handleSummary(data) {
		data.script_name = "%s";
		const res = http.post("http://localhost:8080/internal/summary", JSON.stringify(data), {
			headers: { "Content-Type": "application/json" },
		});
	}
	`, scriptPath)

	finalScript := string(scriptContent) + "\n" + reportingJS

	// 3. Setup the Command with Prometheus Remote Write flag
	cmd := exec.Command(
		"k6", "run", "-",
		"-o", "experimental-prometheus-rw",
	)

	// Feed the injected script via Stdin
	cmd.Stdin = strings.NewReader(finalScript)

	// Optional: Point k6 to your Prometheus server (Update URL if needed)
	cmd.Env = append(os.Environ(), "K6_PROMETHEUS_RW_SERVER_URL=http://localhost:9090/api/v1/write")

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	cmd.Start()

	var goroutineWg sync.WaitGroup

	// STDOUT (Logs only, no regex parsing!)
	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			// Only log actual output, ignore the k6 logo banner
			if strings.TrimSpace(line) != "" && !strings.Contains(line, "k6.io") {
				logger.Log(scriptPath, "INFO", line)
			}
		}
	}()

	// STDERR
	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logger.Log(scriptPath, "ERROR", scanner.Text())
		}
	}()

	cmd.Wait()
	goroutineWg.Wait()

	logger.Log(scriptPath, "INFO", "Execution completed")
}

// ===================== API =====================

// Trigger tests
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

// Receive End-of-Test Summary from k6
func summaryHandler(w http.ResponseWriter, r *http.Request) {
	// Parse the massive JSON payload k6 sends at the end
	var data struct {
		ScriptName string `json:"script_name"`
		Metrics    map[string]struct {
			Values map[string]float64 `json:"values"`
		} `json:"metrics"`
	}

	// Read body to bytes so we can save the raw JSON to the DB too
	bodyBytes, _ := io.ReadAll(r.Body)
	json.Unmarshal(bodyBytes, &data)

	// Extract the specific metrics we care about for the database
	var avgLatency, p95Latency float64
	if reqDuration, exists := data.Metrics["http_req_duration"]; exists {
		avgLatency = reqDuration.Values["avg"]
		p95Latency = reqDuration.Values["p(95)"]
	}

	// Save to TimescaleDB
	_, err := db.Exec(`
		INSERT INTO test_runs (script_name, status, avg_latency, p95_latency, start_time, end_time, raw_summary)
		VALUES ($1, 'COMPLETED', NOW() - INTERVAL '5 minutes', NOW(), $2, $3, $4)`, // Note: start_time is approximated here for simplicity
		data.ScriptName,
		avgLatency,
		p95Latency,
		bodyBytes,
	)

	if err != nil {
		fmt.Printf("Error saving summary to DB: %v\n", err)
		http.Error(w, "Error saving summary", 500)
		return
	}

	fmt.Printf("📊 Summary saved to DB for: %s | P95: %.2fms\n", data.ScriptName, p95Latency)
	w.WriteHeader(200)
}

// ===================== MAIN =====================

func main() {
	initDB()

	mux := http.NewServeMux()
	mux.HandleFunc("/run", runHandler)
	mux.HandleFunc("/internal/summary", summaryHandler) // New endpoint for k6 hook

	fmt.Println("🚀 Runner Agent API running on :8080")
	fmt.Println("➡️  Trigger tests via: POST /run")

	http.ListenAndServe(":8080", mux)
}
