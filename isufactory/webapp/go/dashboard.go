package main

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

var metricNames = []string{"temperature", "vibration", "current"}

type Line struct {
	ID   int64  `db:"id" json:"id"`
	Name string `db:"name" json:"name"`
}

type Reading struct {
	ID         int64     `db:"id"`
	MachineID  int64     `db:"machine_id"`
	Metric     string    `db:"metric"`
	Value      float64   `db:"value"`
	MeasuredAt time.Time `db:"measured_at"`
}

type LatestJSON struct {
	Value      float64   `json:"value"`
	MeasuredAt time.Time `json:"measured_at"`
}

type Assignment struct {
	ID        int64     `db:"id"`
	OrderID   int64     `db:"order_id"`
	MachineID int64     `db:"machine_id"`
	Family    string    `db:"family"`
	StartAt   time.Time `db:"start_at"`
	EndAt     time.Time `db:"end_at"`
	CreatedAt time.Time `db:"created_at"`
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

// latestReadings は設備の指標ごとの最新値を返す。
func latestReadings(machineID int64) (map[string]*LatestJSON, error) {
	latest := map[string]*LatestJSON{}
	for _, metric := range metricNames {
		var r Reading
		err := db.Get(&r, "SELECT * FROM readings WHERE machine_id = ? AND metric = ? ORDER BY measured_at DESC, id DESC LIMIT 1", machineID, metric)
		if errors.Is(err, sql.ErrNoRows) {
			latest[metric] = nil
			continue
		} else if err != nil {
			return nil, err
		}
		latest[metric] = &LatestJSON{Value: r.Value, MeasuredAt: r.MeasuredAt}
	}
	return latest, nil
}

// jobJSON は作業割当に受注・製品の情報を付ける。
func jobJSON(a *Assignment) (*JobJSON, error) {
	var o Order
	if err := db.Get(&o, "SELECT * FROM orders WHERE id = ?", a.OrderID); err != nil {
		return nil, err
	}
	var p Product
	if err := db.Get(&p, "SELECT * FROM products WHERE id = ?", o.ProductID); err != nil {
		return nil, err
	}
	return &JobJSON{
		AssignmentID: a.ID, OrderID: o.ID, ProductID: p.ID, ProductName: p.Name, Family: a.Family,
		Quantity: o.Quantity, StartAt: a.StartAt, EndAt: a.EndAt, DueAt: o.DueAt,
	}, nil
}

func machineStatus(m *Machine, now time.Time) (*MachineStatusJSON, error) {
	latest, err := latestReadings(m.ID)
	if err != nil {
		return nil, err
	}
	var recent int
	if err := db.Get(&recent, "SELECT COUNT(*) FROM alerts WHERE machine_id = ? AND measured_at >= ?", m.ID, now.Add(-alertWindow)); err != nil {
		return nil, err
	}
	st := &MachineStatusJSON{ID: m.ID, Name: m.Name, MachineType: m.MachineType, Latest: latest, RecentAlerts: recent}

	var cur []Assignment
	if err := db.Select(&cur, "SELECT * FROM assignments WHERE machine_id = ? AND start_at <= ? AND end_at > ?", m.ID, now, now); err != nil {
		return nil, err
	}
	if len(cur) > 0 {
		if st.CurrentJob, err = jobJSON(&cur[0]); err != nil {
			return nil, err
		}
	}
	return st, nil
}

type LineStatusJSON struct {
	ID       int64                `json:"id"`
	Name     string               `json:"name"`
	Machines []*MachineStatusJSON `json:"machines"`
}

// getDashboard は全ラインの設備の状態（最新値・直近の異常件数・作業中のジョブ）を返す。
func getDashboard(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	now := time.Now()
	var lines []Line
	if err := db.Select(&lines, "SELECT * FROM `lines` ORDER BY id"); err != nil {
		return err
	}
	res := make([]LineStatusJSON, 0, len(lines))
	for _, l := range lines {
		var machines []Machine
		if err := db.Select(&machines, "SELECT * FROM machines WHERE line_id = ? ORDER BY id", l.ID); err != nil {
			return err
		}
		ls := LineStatusJSON{ID: l.ID, Name: l.Name}
		for i := range machines {
			st, err := machineStatus(&machines[i], now)
			if err != nil {
				return err
			}
			ls.Machines = append(ls.Machines, st)
		}
		res = append(res, ls)
	}
	return c.JSON(http.StatusOK, map[string]any{"lines": res, "generated_at": now})
}

func getMachine(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var m Machine
	if err := db.Get(&m, "SELECT * FROM machines WHERE id = ?", id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "machine not found")
		}
		return err
	}
	st, err := machineStatus(&m, time.Now())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, st)
}

type MetricPointJSON struct {
	Minute time.Time `json:"minute"`
	Avg    float64   `json:"avg"`
	Max    float64   `json:"max"`
	Count  int       `json:"count"`
}

// getMachineMetrics は指標の時系列を 1 分ごとに集計して返す（直近 minutes 分）。
func getMachineMetrics(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	metric := c.QueryParam("metric")
	if !validMetrics[metric] {
		return errorJSON(c, http.StatusBadRequest, "invalid metric")
	}
	minutes, _ := strconv.Atoi(c.QueryParam("minutes"))
	if minutes <= 0 || minutes > 180 {
		minutes = 60
	}
	now := time.Now()
	from := now.Add(-time.Duration(minutes) * time.Minute).Truncate(time.Minute)

	var readings []Reading
	if err := db.Select(&readings, "SELECT * FROM readings WHERE machine_id = ?", id); err != nil {
		return err
	}
	buckets := map[int64]*MetricPointJSON{}
	sums := map[int64]float64{}
	for _, r := range readings {
		if r.Metric != metric || r.MeasuredAt.Before(from) || r.MeasuredAt.After(now) {
			continue
		}
		minute := r.MeasuredAt.Truncate(time.Minute)
		b, ok := buckets[minute.Unix()]
		if !ok {
			b = &MetricPointJSON{Minute: minute, Max: r.Value}
			buckets[minute.Unix()] = b
		}
		b.Count++
		sums[minute.Unix()] += r.Value
		if r.Value > b.Max {
			b.Max = r.Value
		}
	}
	points := make([]MetricPointJSON, 0, len(buckets))
	for k, b := range buckets {
		b.Avg = sums[k] / float64(b.Count)
		points = append(points, *b)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Minute.Before(points[j].Minute) })
	return c.JSON(http.StatusOK, map[string]any{"machine_id": id, "metric": metric, "points": points})
}

// getMachineSchedule は設備の作業予定（1 時間前〜hours 時間後）を返す。
func getMachineSchedule(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	hours, _ := strconv.Atoi(c.QueryParam("hours"))
	if hours <= 0 || hours > 48 {
		hours = 12
	}
	now := time.Now()
	var as []Assignment
	if err := db.Select(&as, "SELECT * FROM assignments WHERE machine_id = ? AND end_at > ? AND start_at < ? ORDER BY start_at",
		id, now.Add(-time.Hour), now.Add(time.Duration(hours)*time.Hour)); err != nil {
		return err
	}
	jobs := make([]*JobJSON, 0, len(as))
	for i := range as {
		j, err := jobJSON(&as[i])
		if err != nil {
			return err
		}
		jobs = append(jobs, j)
	}
	return c.JSON(http.StatusOK, map[string]any{"machine_id": id, "jobs": jobs})
}

func getProductDrawing(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var p Product
	if err := db.Get(&p, "SELECT * FROM products WHERE id = ?", id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "product not found")
		}
		return err
	}
	return c.Blob(http.StatusOK, "image/png", p.Drawing)
}
