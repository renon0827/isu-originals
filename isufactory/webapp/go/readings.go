package main

import (
	"database/sql"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

type Machine struct {
	ID          int64  `db:"id" json:"id"`
	LineID      int64  `db:"line_id" json:"line_id"`
	Name        string `db:"name" json:"name"`
	MachineType string `db:"machine_type" json:"machine_type"`
	Token       string `db:"token" json:"-"`
}

var validMetrics = map[string]bool{"temperature": true, "vibration": true, "current": true}

const (
	// 異常判定: 直近 10 分の平均の 1.5 倍を超えたら異常（平均の計算には 5 件以上必要）
	alertWindow    = 10 * time.Minute
	alertRatio     = 1.5
	alertMinSample = 5
	// 送信端末の時計のずれとして許容する範囲
	maxClockSkew = 5 * time.Minute
	maxBatchSize = 100
)

type ReadingInput struct {
	Metric     string    `json:"metric"`
	Value      float64   `json:"value"`
	MeasuredAt time.Time `json:"measured_at"`
}

type ReadingsRequest struct {
	Readings []ReadingInput `json:"readings"`
}

// postReadings はセンサー送信端末から計測値をまとめて受け取る。
func postReadings(c echo.Context) error {
	token := c.Request().Header.Get("X-Machine-Token")
	var m Machine
	if err := db.Get(&m, "SELECT * FROM machines WHERE token = ?", token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusUnauthorized, "invalid machine token")
		}
		return err
	}
	var req ReadingsRequest
	if err := c.Bind(&req); err != nil || len(req.Readings) == 0 || len(req.Readings) > maxBatchSize {
		return errorJSON(c, http.StatusBadRequest, "readings must contain 1 to 100 items")
	}
	now := time.Now()
	for _, r := range req.Readings {
		if !validMetrics[r.Metric] || math.IsNaN(r.Value) || math.IsInf(r.Value, 0) ||
			r.MeasuredAt.Before(now.Add(-maxClockSkew)) || r.MeasuredAt.After(now.Add(maxClockSkew)) {
			return errorJSON(c, http.StatusBadRequest, "invalid reading")
		}
	}

	for _, r := range req.Readings {
		if _, err := db.Exec("INSERT INTO readings (machine_id, metric, value, measured_at) VALUES (?, ?, ?, ?)",
			m.ID, r.Metric, r.Value, r.MeasuredAt); err != nil {
			return err
		}
		if err := detectAnomaly(m.ID, r); err != nil {
			return err
		}
	}
	return c.JSON(http.StatusCreated, map[string]int{"accepted": len(req.Readings)})
}

// detectAnomaly は計測値が直近 10 分の平均から大きく外れていればアラートを記録する。
func detectAnomaly(machineID int64, r ReadingInput) error {
	var stat struct {
		Avg   sql.NullFloat64 `db:"avg"`
		Count int             `db:"cnt"`
	}
	if err := db.Get(&stat,
		"SELECT AVG(value) AS avg, COUNT(*) AS cnt FROM readings WHERE machine_id = ? AND metric = ? AND measured_at >= ? AND measured_at < ?",
		machineID, r.Metric, r.MeasuredAt.Add(-alertWindow), r.MeasuredAt); err != nil {
		return err
	}
	if stat.Count < alertMinSample || !stat.Avg.Valid || r.Value <= stat.Avg.Float64*alertRatio {
		return nil
	}
	_, err := db.Exec("INSERT INTO alerts (machine_id, metric, value, baseline, measured_at, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		machineID, r.Metric, r.Value, stat.Avg.Float64, r.MeasuredAt, time.Now())
	return err
}

type Alert struct {
	ID         int64     `db:"id" json:"id"`
	MachineID  int64     `db:"machine_id" json:"machine_id"`
	Metric     string    `db:"metric" json:"metric"`
	Value      float64   `db:"value" json:"value"`
	Baseline   float64   `db:"baseline" json:"baseline"`
	MeasuredAt time.Time `db:"measured_at" json:"measured_at"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// getAlerts は新しい順にアラートを返す。machine_id で絞り込める。
func getAlerts(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var alerts []Alert
	if mid := c.QueryParam("machine_id"); mid != "" {
		err = db.Select(&alerts, "SELECT * FROM alerts WHERE machine_id = ? ORDER BY measured_at DESC, id DESC LIMIT ?", mid, limit)
	} else {
		err = db.Select(&alerts, "SELECT * FROM alerts ORDER BY measured_at DESC, id DESC LIMIT ?", limit)
	}
	if err != nil {
		return err
	}
	if alerts == nil {
		alerts = []Alert{}
	}
	return c.JSON(http.StatusOK, map[string]any{"alerts": alerts})
}
