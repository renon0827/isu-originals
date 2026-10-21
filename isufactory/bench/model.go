package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

func newGzipReader(r io.Reader) (*gzip.Reader, error) { return gzip.NewReader(r) }

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ---- 初期データの前提（tools/datagen と一致させる） ----

const (
	numLines     = 10
	numOperators = 50
	numPlanners  = 10
	setupTime    = 600 * time.Second
)

var (
	machineTypes = []string{"press", "weld", "paint", "assembly"}
	metricNames  = []string{"temperature", "vibration", "current"}
	metricBase   = map[string]float64{"temperature": 60, "vibration": 3, "current": 12}
)

func operatorLogin(i int) string     { return fmt.Sprintf("operator%02d", i) }
func plannerLogin(i int) string      { return fmt.Sprintf("planner%02d", i) }
func passwordOf(login string) string { return "isu-" + login }

func machineToken(id int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("isufactory-machine-%d", id)))
	return hex.EncodeToString(h[:])[:32]
}

// ---- API の JSON ----

type UserJSON struct {
	ID        int64  `json:"id"`
	LoginName string `json:"login_name"`
	Role      string `json:"role"`
}

type LatestJSON struct {
	Value      float64   `json:"value"`
	MeasuredAt time.Time `json:"measured_at"`
}

type JobJSON struct {
	AssignmentID int64     `json:"assignment_id"`
	OrderID      int64     `json:"order_id"`
	ProductID    int64     `json:"product_id"`
	ProductName  string    `json:"product_name"`
	Family       string    `json:"family"`
	Quantity     int       `json:"quantity"`
	StartAt      time.Time `json:"start_at"`
	EndAt        time.Time `json:"end_at"`
	DueAt        time.Time `json:"due_at"`
}

type MachineStatusJSON struct {
	ID           int64                  `json:"id"`
	Name         string                 `json:"name"`
	MachineType  string                 `json:"machine_type"`
	Latest       map[string]*LatestJSON `json:"latest"`
	RecentAlerts int                    `json:"recent_alerts"`
	CurrentJob   *JobJSON               `json:"current_job"`
}

type DashboardJSON struct {
	Lines []struct {
		ID       int64                `json:"id"`
		Name     string               `json:"name"`
		Machines []*MachineStatusJSON `json:"machines"`
	} `json:"lines"`
}

type MetricsJSON struct {
	MachineID int64  `json:"machine_id"`
	Metric    string `json:"metric"`
	Points    []struct {
		Minute time.Time `json:"minute"`
		Avg    float64   `json:"avg"`
		Max    float64   `json:"max"`
		Count  int       `json:"count"`
	} `json:"points"`
}

type ScheduleJSON struct {
	MachineID int64      `json:"machine_id"`
	Jobs      []*JobJSON `json:"jobs"`
}

type AlertJSON struct {
	ID         int64     `json:"id"`
	MachineID  int64     `json:"machine_id"`
	Metric     string    `json:"metric"`
	Value      float64   `json:"value"`
	Baseline   float64   `json:"baseline"`
	MeasuredAt time.Time `json:"measured_at"`
}

type BOMItem struct {
	MaterialID int64 `json:"material_id"`
	Quantity   int   `json:"quantity"`
}

type ProductJSON struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Family      string    `json:"family"`
	MachineType string    `json:"machine_type"`
	UnitSeconds int       `json:"unit_seconds"`
	BOM         []BOMItem `json:"bom"`
}

type MaterialJSON struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Stock int64  `json:"stock"`
}

type PlannedJSON struct {
	OrderID   int64     `json:"order_id"`
	MachineID int64     `json:"machine_id"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
}

type PlanResponse struct {
	Assignments []PlannedJSON `json:"assignments"`
	Skipped     []int64       `json:"skipped"`
}
