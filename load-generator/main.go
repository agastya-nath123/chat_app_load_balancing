package main

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Result struct {
	Experiment     string  `json:"experiment"`
	Requests       int     `json:"requests"`
	Concurrency    int     `json:"concurrency"`
	Successful     int     `json:"successful"`
	Failed         int     `json:"failed"`
	ThroughputRPS  float64 `json:"throughput_rps"`
	DropoutPercent float64 `json:"dropout_percent"`
	P50MS          float64 `json:"p50_ms"`
	P95MS          float64 `json:"p95_ms"`
	P99MS          float64 `json:"p99_ms"`
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}

	index := int(float64(len(values)-1) * p)

	return values[index]
}

func calculatePercentiles(
	latencies []time.Duration,
) (time.Duration, time.Duration, time.Duration) {

	if len(latencies) == 0 {
		return 0, 0, 0
	}

	values := append([]time.Duration(nil), latencies...)

	sort.Slice(values, func(i, j int) bool {
		return values[i] < values[j]
	})

	return percentile(values, 0.50),
		percentile(values, 0.95),
		percentile(values, 0.99)
}

func writeJSON(path string, result Result) error {
	if path == "" {
		return nil
	}

	dir := filepath.Dir(path)

	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}

	data = append(data, '\n')

	return os.WriteFile(path, data, 0644)
}

func writeCSV(path string, result Result) error {
	if path == "" {
		return nil
	}

	dir := filepath.Dir(path)

	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	fileExists := false

	if _, err := os.Stat(path); err == nil {
		fileExists = true
	}

	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if !fileExists {
		err := writer.Write([]string{
			"experiment",
			"requests",
			"concurrency",
			"successful",
			"failed",
			"throughput_rps",
			"dropout_percent",
			"p50_ms",
			"p95_ms",
			"p99_ms",
		})

		if err != nil {
			return err
		}
	}

	return writer.Write([]string{
		result.Experiment,
		fmt.Sprintf("%d", result.Requests),
		fmt.Sprintf("%d", result.Concurrency),
		fmt.Sprintf("%d", result.Successful),
		fmt.Sprintf("%d", result.Failed),
		fmt.Sprintf("%.3f", result.ThroughputRPS),
		fmt.Sprintf("%.3f", result.DropoutPercent),
		fmt.Sprintf("%.3f", result.P50MS),
		fmt.Sprintf("%.3f", result.P95MS),
		fmt.Sprintf("%.3f", result.P99MS),
	})
}

func main() {
	target := flag.String(
		"url",
		"",
		"WebSocket URL of the load balancer",
	)

	requests := flag.Int(
		"requests",
		100,
		"Number of WebSocket connections to create",
	)

	concurrency := flag.Int(
		"concurrency",
		10,
		"Maximum number of concurrent connections",
	)

	experiment := flag.String(
		"experiment",
		"baseline",
		"Experiment name",
	)

	timeout := flag.Duration(
		"timeout",
		5*time.Second,
		"Timeout for each WebSocket connection attempt",
	)

	out := flag.String(
		"out",
		"",
		"JSON output file",
	)

	csvPath := flag.String(
		"csv",
		"",
		"Cumulative CSV output file",
	)

	flag.Parse()

	if *target == "" {
		log.Fatal("missing -url")
	}

	if *requests <= 0 {
		log.Fatal("requests must be greater than 0")
	}

	if *concurrency <= 0 {
		log.Fatal("concurrency must be greater than 0")
	}

	if *timeout <= 0 {
		log.Fatal("timeout must be greater than 0")
	}

	if *concurrency > *requests {
		*concurrency = *requests
	}

	var (
		wg sync.WaitGroup
		mu sync.Mutex

		successful int
		failed     int

		latencies []time.Duration
	)

	// The backend certificates are self-signed in your lab setup.
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	// Limit the number of simultaneously active connections.
	sem := make(chan struct{}, *concurrency)

	start := time.Now()

	for i := 0; i < *requests; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sem <- struct{}{}
			defer func() {
				<-sem
			}()

			requestStart := time.Now()

			ctx, cancel := context.WithTimeout(
				context.Background(),
				*timeout,
			)
			defer cancel()

			type dialResult struct {
				conn *websocket.Conn
				err  error
			}

			resultCh := make(chan dialResult, 1)

			go func() {
				conn, _, err := dialer.Dial(*target, nil)

				resultCh <- dialResult{
					conn: conn,
					err:  err,
				}
			}()

			var conn *websocket.Conn
			var err error

			select {
			case result := <-resultCh:
				conn = result.conn
				err = result.err

			case <-ctx.Done():
				err = ctx.Err()
			}

			elapsed := time.Since(requestStart)

			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}

			conn.SetReadDeadline(time.Now().Add(*timeout))

			_, _, readErr := conn.ReadMessage()

			mu.Lock()

			if readErr != nil {
				if websocket.IsCloseError(
					readErr,
					1013,
				) {
					// Synthetic backend failure.
					failed++
					mu.Unlock()

					conn.Close()
					return
				}

				// Other connection-level failure.
				failed++
				mu.Unlock()

				conn.Close()
				return
			}

			// Connection stayed open, so consider it successful.
			successful++
			latencies = append(latencies, elapsed)

			mu.Unlock()

			conn.Close()

		}()

	}

	wg.Wait()

	elapsed := time.Since(start)

	p50, p95, p99 := calculatePercentiles(latencies)

	var throughput float64
	if elapsed > 0 {
		throughput =
			float64(successful) /
				elapsed.Seconds()
	}

	var dropout float64
	if *requests > 0 {
		dropout =
			float64(failed) /
				float64(*requests) *
				100
	}

	result := Result{
		Experiment:     *experiment,
		Requests:       *requests,
		Concurrency:    *concurrency,
		Successful:     successful,
		Failed:         failed,
		ThroughputRPS:  throughput,
		DropoutPercent: dropout,
		P50MS:          float64(p50) / float64(time.Millisecond),
		P95MS:          float64(p95) / float64(time.Millisecond),
		P99MS:          float64(p99) / float64(time.Millisecond),
	}

	data, err := json.MarshalIndent(
		result,
		"",
		"  ",
	)

	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(string(data))

	if err := writeJSON(*out, result); err != nil {
		log.Fatalf("failed to write JSON: %v", err)
	}

	if err := writeCSV(*csvPath, result); err != nil {
		log.Fatalf("failed to write CSV: %v", err)
	}
}
