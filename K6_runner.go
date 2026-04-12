package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

// ===================== LOGGER =====================

type Logger struct {
	file  *os.File
	mutex sync.Mutex
}

func NewLogger(script string) *Logger {
	os.MkdirAll("logs", os.ModePerm)

	filePath := fmt.Sprintf("logs/%s.log", script)

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
	pushLogToLoki(script, string(jsonData))
}

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

	if strings.Contains(line, "http_reqs") {
		re := regexp.MustCompile(`http_reqs.*:\s+(\d+)`)
		match := re.FindStringSubmatch(line)
		if len(match) > 1 {
			val, _ := strconv.Atoi(match[1])
			m.Requests = val
		}
	}

	if strings.Contains(line, "http_req_duration") {

		avgRe := regexp.MustCompile(`avg=([\d\.]+)([a-z]+)`)
		avgMatch := avgRe.FindStringSubmatch(line)

		if len(avgMatch) > 1 {
			avg, _ := strconv.ParseFloat(avgMatch[1], 64)
			if avgMatch[2] == "s" {
				avg *= 1000
			}
			m.AvgLatency = avg
		}

		p95Re := regexp.MustCompile(`p$begin:math:text$95$end:math:text$\s*=\s*([\d\.]+)([a-z]+)`)
		p95Match := p95Re.FindStringSubmatch(line)

		if len(p95Match) > 1 {
			p95, _ := strconv.ParseFloat(p95Match[1], 64)
			if p95Match[2] == "s" {
				p95 *= 1000
			}
			m.P95Latency = p95
		}
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

	logger := NewLogger(script)
	if logger == nil {
		return
	}
	defer logger.file.Close()

	logger.Log(script, "INFO", "Starting execution")

	cmd := exec.Command("k6", "run", script)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	cmd.Start()

	metrics := &Metrics{}
	var goroutineWg sync.WaitGroup

	// STDOUT
	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			logger.Log(script, "INFO", line)
			parseMetrics(line, metrics)
		}
	}()

	// STDERR
	goroutineWg.Add(1)
	go func() {
		defer goroutineWg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logger.Log(script, "ERROR", scanner.Text())
		}
	}()

	cmd.Wait()
	goroutineWg.Wait()

	// Prometheus
	requestsGauge.WithLabelValues(script).Set(float64(metrics.Requests))
	latencyGauge.WithLabelValues(script).Set(metrics.AvgLatency)
	p95Gauge.WithLabelValues(script).Set(metrics.P95Latency)

	// TimescaleDB
	storeMetrics(script, metrics)

	logger.Log(script, "INFO", "Execution completed")
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

// ===================== MAIN =====================

func main() {

	initDB()

	// Prometheus
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		fmt.Println("📡 Metrics running on :2112")
		http.ListenAndServe(":2112", mux)
	}()

	// API
	mux := http.NewServeMux()
	mux.HandleFunc("/run", runHandler)

	fmt.Println("🚀 API running on :8080")
	http.ListenAndServe(":8080", mux)
}
