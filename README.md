# 🚀 K6 Observability & Load Testing Platform

![Go](https://img.shields.io/badge/Go-1.21-blue?logo=go)
![k6](https://img.shields.io/badge/k6-Load%20Testing-purple)
![Prometheus](https://img.shields.io/badge/Prometheus-Metrics-orange?logo=prometheus)
![Grafana](https://img.shields.io/badge/Grafana-Dashboard-yellow?logo=grafana)
![Loki](https://img.shields.io/badge/Loki-Logs-blueviolet)
![TimescaleDB](https://img.shields.io/badge/TimescaleDB-TimeSeries-green)

---

## 🧠 Overview

A **production-style load testing and observability platform** that:

- Runs **k6 scripts dynamically via API**
- Streams logs to **Loki**
- Exposes metrics to **Prometheus**
- Visualizes everything in **Grafana**
- Stores historical data in **TimescaleDB (ML-ready)**

---

## 🏗️ System Architecture

<img width="300" height="300" alt="Media Input Analysis-2026-04-11-204146" src="https://github.com/user-attachments/assets/7b6db3b7-db99-46dd-8057-2d3f69047576" />

⸻

⚙️ Tech Stack

Layer Technology
Load Testing k6
Backend Go
Metrics Prometheus
Logs Loki
Visualization Grafana
Storage TimescaleDB

⸻

🚀 Features

🔥 Core
• Dynamic script execution via API
• Multi-script parallel execution
• Real-time logs streaming
• Metrics scraping with Prometheus
• SQL-based analytics via TimescaleDB

🚀 Advanced
• Observability pipeline (Metrics + Logs + DB)
• Time-series storage
• ML-ready dataset generation
• Grafana dashboards (combined view)

⸻

📦 Project Structure

.
├── main.go
├── scripts/
│ ├── login.js
│ ├── search.js
│ └── payment.js
├── Dockerfile
├── docker-compose.yml
└── README.md

⸻

🚀 Getting Started

1️⃣ Clone

```bash
git clone <your-repo-url>
cd project
```

⸻

2️⃣ Run system

```bash
docker-compose up --build
```

⸻

🌐 Services

Service URL

```
API	http://localhost:8080
Prometheus	http://localhost:9090
Grafana	http://localhost:3000
Metrics	http://localhost:2112/metrics
```

⸻

📡 API Documentation

▶️ Run Load Test

Endpoint

```
POST /run
```

⸻

Request Body

```
["login.js", "search.js"]
```

⸻
Example

```
curl -X POST http://localhost:8080/run \
  -H "Content-Type: application/json" \
  -d '["login.js","search.js"]'

```

⸻

Response

```
"✅ Execution started"

```

⸻

```
🧠 Behavior
	•	Executes scripts concurrently
	•	Streams logs to Loki
	•	Updates Prometheus metrics
	•	Stores results in TimescaleDB
```

⸻

```
📊 Metrics (Prometheus)

Available metrics:
	•	k6_requests_total
	•	k6_latency_avg_ms
	•	k6_latency_p95_ms
```

⸻

📜 Logs (Loki)

Query logs in Grafana:

```
{job="k6"}

Filter by script:

{job="k6", script="login.js"}

```

⸻

🗄️ TimescaleDB

Schema

```
CREATE TABLE metrics (
    time TIMESTAMPTZ NOT NULL,
    script TEXT,
    requests INT,
    avg_latency FLOAT,
    p95_latency FLOAT
);

```

⸻

Convert to hypertable

```
SELECT create_hypertable('metrics', 'time');
```

⸻

Useful Queries

Recent Data

```
SELECT * FROM metrics ORDER BY time DESC LIMIT 10;
```

⸻

Avg Latency

```
SELECT script, AVG(avg_latency)
FROM metrics
GROUP BY script;
```

⸻

Time Aggregation

```
SELECT time_bucket('1 minute', time) AS bucket,
       script,
       AVG(avg_latency)
FROM metrics
GROUP BY bucket, script;
```

---

## 📈 Grafana Dashboards

### Panels

- Requests per script
- Avg latency
- P95 latency
- Logs panel (Loki)
- SQL analytics (TimescaleDB)

---

## 🔥 Combined Observability

- Correlate metrics + logs
- Debug latency spikes instantly
- Analyze historical trends

---

## ⚠️ Limitations

- Regex-based parsing (fragile)
- Uses Gauge instead of Counter
- Summary-based metrics (not real-time per request)

---

## 🚀 Future Improvements

### 🔥 Metrics

- Prometheus Counters & Histograms
- Real-time RPS

### 🔥 Parsing

- Switch to k6 JSON output

### 🔥 Architecture

- Job queue system
- Distributed runners
- Script upload API

### 🔥 ML

- Anomaly detection
- Forecasting
- Clustering

---

## 🧠 Key Learnings

- Observability = Metrics + Logs + Storage
- Prometheus ≠ long-term DB
- TimescaleDB enables ML workflows
- API-driven load testing = scalable design

---

## 👨‍💻 Author

Built as a **production-style system** to explore:

- Observability Engineering
- Distributed Systems
- Data Pipelines
- ML-ready infrastructure

---

⭐ Support

If you like this project:

👉 Star ⭐ the repo
👉 Share 🚀
👉 Build further 💪

---
