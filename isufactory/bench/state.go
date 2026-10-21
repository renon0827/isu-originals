package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ---- スコア ----

const (
	readingsPerPoint = 5  // 受理された計測値 5 件で 1 点
	pointsDashboard  = 5  // ダッシュボード 1 回
	pointsMachine    = 2  // 設備詳細・時系列・予定の各 1 回
	pointsPerOrder   = 30 // 納期どおりに計画された受注 1 件（遅れると減る）
	lateZero         = 2 * time.Hour
)

type Score struct {
	mu sync.Mutex

	Readings     int64
	Views        int64
	PlanPoints   float64
	OrdersOnTime int64
	OrdersLate   int64

	penalties map[string]*penaltyAgg
	messages  []string

	failed     atomic.Bool
	failReason atomic.Value
}

type penaltyAgg struct {
	count  int
	points int
}

func NewScore() *Score { return &Score{penalties: map[string]*penaltyAgg{}} }

func (s *Score) AddReadings(n int) {
	s.mu.Lock()
	s.Readings += int64(n)
	s.mu.Unlock()
}

func (s *Score) AddView(points int) {
	s.mu.Lock()
	s.Views += int64(points)
	s.mu.Unlock()
}

// AddPlan は計画された受注の得点を加える。納期遅れに応じて減点し、2 時間遅れで 0 点。
func (s *Score) AddPlan(lateness time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lateness <= 0 {
		s.PlanPoints += pointsPerOrder
		s.OrdersOnTime++
		return
	}
	s.OrdersLate++
	f := 1 - float64(lateness)/float64(lateZero)
	if f > 0 {
		s.PlanPoints += pointsPerOrder * f
	}
}

func (s *Score) Penalize(reason string, points int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.penalties[reason]
	if !ok {
		a = &penaltyAgg{}
		s.penalties[reason] = a
	}
	a.count++
	a.points += points
}

func (s *Score) Fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if s.failed.CompareAndSwap(false, true) {
		s.failReason.Store(msg)
	}
	s.Message("[FAIL] " + msg)
}

func (s *Score) Failed() (bool, string) {
	if !s.failed.Load() {
		return false, ""
	}
	r, _ := s.failReason.Load().(string)
	return true, r
}

func (s *Score) Message(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) < 200 {
		s.messages = append(s.messages, time.Now().Format("15:04:05.000")+" "+m)
	}
}

type Result struct {
	Pass          bool           `json:"pass"`
	Score         int64          `json:"score"`
	ReadingPoints int64          `json:"reading_points"`
	ViewPoints    int64          `json:"view_points"`
	PlanPoints    int64          `json:"plan_points"`
	Readings      int64          `json:"readings"`
	OrdersOnTime  int64          `json:"orders_on_time"`
	OrdersLate    int64          `json:"orders_late"`
	PenaltyPoints int64          `json:"penalty_points"`
	Penalties     map[string]int `json:"penalties"`
	FailReason    string         `json:"fail_reason,omitempty"`
	Messages      []string       `json:"messages"`
}

func (s *Score) Result() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Result{
		ReadingPoints: s.Readings / readingsPerPoint,
		ViewPoints:    s.Views,
		PlanPoints:    int64(s.PlanPoints),
		Readings:      s.Readings,
		OrdersOnTime:  s.OrdersOnTime,
		OrdersLate:    s.OrdersLate,
		Penalties:     map[string]int{},
		Messages:      s.messages,
	}
	for reason, a := range s.penalties {
		r.PenaltyPoints += int64(a.points)
		r.Penalties[reason] = a.count
	}
	r.Score = r.ReadingPoints + r.ViewPoints + r.PlanPoints - r.PenaltyPoints
	if r.Score < 0 {
		r.Score = 0
	}
	if failed, reason := s.Failed(); failed {
		r.Score = 0
		r.FailReason = reason
	} else {
		r.Pass = true
	}
	return r
}

func (s *Score) PenaltySummary() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for reason, a := range s.penalties {
		out = append(out, fmt.Sprintf("%s: %d 件 (-%d)", reason, a.count, a.points))
	}
	sort.Strings(out)
	return out
}

// ---- 計測値の記録 ----

type sentReading struct {
	metric     string
	value      float64
	measuredAt time.Time
	spike      bool
}

// MachineState はベンチマーカーが送った計測値と、その受理状況を設備ごとに持つ。
type MachineState struct {
	ID    int64
	Type  string
	Token string

	mu sync.Mutex
	// 受理が確定した計測値（受理を確認した時刻つき）
	acked []ackedReading
	// 結果が分からない（タイムアウト等）計測値の時刻
	unknown []time.Time
}

type ackedReading struct {
	sentReading
	ackedAt time.Time
}

func (m *MachineState) addAcked(rs []sentReading, at time.Time) {
	m.mu.Lock()
	for _, r := range rs {
		m.acked = append(m.acked, ackedReading{r, at})
	}
	m.mu.Unlock()
}

func (m *MachineState) addUnknown(rs []sentReading) {
	m.mu.Lock()
	for _, r := range rs {
		m.unknown = append(m.unknown, r.measuredAt)
	}
	m.mu.Unlock()
}

// latestAckedBefore は t より前に受理が確定した計測値のうち、指標ごとに最新の計測時刻を返す。
func (m *MachineState) latestAckedBefore(t time.Time) map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := map[string]time.Time{}
	for _, r := range m.acked {
		if r.ackedAt.Before(t) && r.measuredAt.After(res[r.metric]) {
			res[r.metric] = r.measuredAt
		}
	}
	return res
}

// windowStats は [from, to) に計測され、ackedBy までに受理が確定した計測値の件数と最大値。
func (m *MachineState) windowStats(metric string, from, to, ackedBy time.Time) (int, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, max := 0, 0.0
	for _, r := range m.acked {
		if r.metric == metric && !r.measuredAt.Before(from) && r.measuredAt.Before(to) && r.ackedAt.Before(ackedBy) {
			n++
			if r.value > max {
				max = r.value
			}
		}
	}
	return n, max
}

// recentNormals は直近 10 分に送った（受理済みの）通常値の件数。異常値を混ぜてよいかの判断に使う。
func (m *MachineState) recentNormals(metric string, now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.acked {
		if r.metric == metric && !r.spike && r.measuredAt.After(now.Add(-9*time.Minute)) {
			n++
		}
	}
	return n
}

// isKnownSpike は計測時刻が、ベンチが送った異常値（受理済み・結果不明を含む）か。
func (m *MachineState) isKnownSpike(metric string, t time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.acked {
		if r.spike && r.metric == metric && r.measuredAt.Equal(t) {
			return true
		}
	}
	for _, u := range m.unknown {
		if u.Equal(t) {
			return true
		}
	}
	return false
}

// ---- 受注・計画 ----

type OrderState struct {
	ID        int64
	Product   *ProductJSON
	Quantity  int
	DueAt     time.Time
	Planned   *PlannedJSON
	CreatedAt time.Time
}
