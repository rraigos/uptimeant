// UptimeAnt agent: posts this VPS's CPU/RAM/disk usage to the server every N minutes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type report struct {
	CPU  float64 `json:"cpu"`
	RAM  float64 `json:"ram"`
	Disk float64 `json:"disk"`
	Host string  `json:"host"`
}

type cpuTimes struct{ idle, total uint64 }

func main() {
	baseURL := strings.TrimRight(os.Getenv("UPTIMEANT_URL"), "/")
	token := os.Getenv("UPTIMEANT_TOKEN")
	diskPath := os.Getenv("UPTIMEANT_DISK_PATH")
	if diskPath == "" {
		diskPath = "/"
	}
	minutes, err := strconv.Atoi(os.Getenv("UPTIMEANT_INTERVAL"))
	if err != nil || minutes < 1 {
		minutes = 10
	}
	if baseURL == "" || token == "" {
		log.Fatal("UPTIMEANT_URL and UPTIMEANT_TOKEN are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	host, _ := os.Hostname()
	client := &http.Client{Timeout: 15 * time.Second}

	prev, err := readCPU()
	if err != nil {
		log.Fatalf("metrics are not available: %v", err)
	}
	send := func(first bool) {
		if first {
			time.Sleep(time.Second) // the first report measures CPU over 1 second
		}
		cur, err := readCPU()
		if err != nil {
			log.Printf("cpu: %v", err)
			return
		}
		cpu := 0.0
		if dt := cur.total - prev.total; dt > 0 {
			cpu = 100 * (1 - float64(cur.idle-prev.idle)/float64(dt))
		}
		prev = cur
		ram, err := readMem()
		if err != nil {
			log.Printf("ram: %v", err)
			return
		}
		disk, err := readDisk(diskPath)
		if err != nil {
			log.Printf("disk: %v", err)
			return
		}
		body, _ := json.Marshal(report{CPU: clamp(cpu), RAM: clamp(ram), Disk: clamp(disk), Host: host})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/agent/report", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("send failed: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("server answered %s", resp.Status)
			return
		}
		log.Printf("report sent: %s", fmt.Sprintf("CPU %.0f%% RAM %.0f%% disk %.0f%%", cpu, ram, disk))
	}

	send(true)
	t := time.NewTicker(time.Duration(minutes) * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send(false)
		}
	}
}

func clamp(v float64) float64 {
	return min(100, max(0, v))
}
