package main

import (
	"crypto/rsa"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// BenchEvent はベンチマーカーが走行用に作成した公演。
type BenchEvent struct {
	ID         int64
	SaleType   string
	SaleStart  time.Time
	SaleEnd    time.Time
	AnnounceAt time.Time
	GateOpen   time.Time
	EndAt      time.Time
	Prices     map[string]int
	SeatsTotal map[string]int // ランク別の総座席数

	mu sync.Mutex
	// 確定した販売（レスポンスを受け取った時刻とランク別枚数）
	sales []saleRecord
	// 抽選: 申込（受理されたもの）
	entries []*LotteryEntry
	// 抽選結果を最初に確認できた時刻
	resultSeenAt time.Time
}

type saleRecord struct {
	at   time.Time
	rank string
	n    int
}

type LotteryEntry struct {
	Fan       *Fan
	EventID   int64
	Rank      string
	Quantity  int
	EnteredAt time.Time
	Status    string // pending / won / lost
	// 当選チケットを取り込み済みか
	Collected bool
}

func (e *BenchEvent) recordSale(at time.Time, rank string, n int) {
	e.mu.Lock()
	e.sales = append(e.sales, saleRecord{at, rank, n})
	e.mu.Unlock()
}

// soldBefore は t より前に確定した販売数（ランク別）。
func (e *BenchEvent) soldBefore(t time.Time) map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := map[string]int{}
	for _, s := range e.sales {
		if s.at.Before(t) {
			m[s.rank] += s.n
		}
	}
	return m
}

func (e *BenchEvent) soldTotal() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.sales {
		n += s.n
	}
	return n
}

// ---- スコアと検証結果 ----

type Penalty struct {
	Reason string
	Points int
}

type Score struct {
	mu sync.Mutex

	RevenueFirstCome int64
	RevenueLottery   int64
	ResaleFee        int64
	Entries          int64

	PurchaseSuccess  int64
	PurchaseConflict int64
	ResaleSuccess    int64

	penalties map[string]*penaltyAgg

	failed     atomic.Bool
	failReason atomic.Value

	messages []string
}

type penaltyAgg struct {
	count  int
	points int
}

func NewScore() *Score {
	return &Score{penalties: map[string]*penaltyAgg{}}
}

func (s *Score) AddRevenue(kind string, amount int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "first_come":
		s.RevenueFirstCome += int64(amount)
	case "lottery":
		s.RevenueLottery += int64(amount)
	case "resale_fee":
		s.ResaleFee += int64(amount)
	}
}

func (s *Score) Inc(p *int64) {
	s.mu.Lock()
	*p++
	s.mu.Unlock()
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

// Fail は致命的な整合性違反。スコアは 0 になる。
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

const (
	pointsPerEntry = 30
	revenueDivisor = 100
)

type Result struct {
	Pass             bool           `json:"pass"`
	Score            int64          `json:"score"`
	RevenueFirstCome int64          `json:"revenue_first_come"`
	RevenueLottery   int64          `json:"revenue_lottery"`
	ResaleFee        int64          `json:"resale_fee"`
	Entries          int64          `json:"entries"`
	PurchaseSuccess  int64          `json:"purchase_success"`
	PurchaseConflict int64          `json:"purchase_conflict"`
	ResaleSuccess    int64          `json:"resale_success"`
	PenaltyPoints    int64          `json:"penalty_points"`
	Penalties        map[string]int `json:"penalties"`
	FailReason       string         `json:"fail_reason,omitempty"`
	Messages         []string       `json:"messages"`
}

func (s *Score) Result() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Result{
		RevenueFirstCome: s.RevenueFirstCome,
		RevenueLottery:   s.RevenueLottery,
		ResaleFee:        s.ResaleFee,
		Entries:          s.Entries,
		PurchaseSuccess:  s.PurchaseSuccess,
		PurchaseConflict: s.PurchaseConflict,
		ResaleSuccess:    s.ResaleSuccess,
		Penalties:        map[string]int{},
		Messages:         s.messages,
	}
	for reason, a := range s.penalties {
		r.PenaltyPoints += int64(a.points)
		r.Penalties[reason] = a.count
	}
	base := (s.RevenueFirstCome+s.RevenueLottery+s.ResaleFee)/revenueDivisor + s.Entries*pointsPerEntry
	r.Score = base - r.PenaltyPoints
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

// ---- 全体の状態 ----

type State struct {
	cfg     Config
	payment *PaymentServer
	score   *Score
	stats   Stats
	pubKey  *rsa.PublicKey

	T0 time.Time

	events    map[int64]*BenchEvent
	fcEvents  []*BenchEvent
	lotEvents []*BenchEvent
	prices    sync.Map // eventID → map[string]int（初期データの公演を含む）

	seatMu    sync.Mutex
	soldSeats map[int64]int64 // seatID → ticketID（ベンチが確認した販売）

	payMu          sync.Mutex
	matchedPayment map[string]bool
	unknownOutcome int

	listingMu sync.Mutex
	listings  map[int64]*Listing

	nextFan atomic.Int64
}

type Listing struct {
	ID       int64
	EventID  int64
	TicketID int64
	Seller   *Fan
	Price    int
	SoldTo   *Fan
	OldQR    string
}

func NewState(cfg Config, payment *PaymentServer) *State {
	return &State{
		cfg:            cfg,
		payment:        payment,
		score:          NewScore(),
		events:         map[int64]*BenchEvent{},
		soldSeats:      map[int64]int64{},
		matchedPayment: map[string]bool{},
		listings:       map[int64]*Listing{},
	}
}

// markSeatsSold は販売済み座席を記録し、二重販売を検出する。
func (st *State) markSeatsSold(eventID int64, tickets map[int64]int64) {
	st.seatMu.Lock()
	defer st.seatMu.Unlock()
	for seatID, ticketID := range tickets {
		if prev, ok := st.soldSeats[seatID]; ok && prev != ticketID {
			st.score.Fail("座席 %d (公演 %d) が二重に販売されました (ticket %d, %d)", seatID, eventID, prev, ticketID)
			continue
		}
		st.soldSeats[seatID] = ticketID
	}
}

// checkPayment は購入レスポンスの payment_id が決済サービスの記録と一致するか検証する。
func (st *State) checkPayment(paymentID string, total int) bool {
	p, ok := st.payment.Get(paymentID)
	if !ok {
		st.score.Fail("存在しない payment_id %q が返されました", paymentID)
		return false
	}
	if p.Amount != total {
		st.score.Fail("決済額 (%d) と注文金額 (%d) が一致しません", p.Amount, total)
		return false
	}
	st.payMu.Lock()
	defer st.payMu.Unlock()
	if st.matchedPayment[paymentID] {
		st.score.Fail("同じ決済 %q が複数の注文に使われました", paymentID)
		return false
	}
	st.matchedPayment[paymentID] = true
	return true
}

// unknownPurchaseOutcome は結果が分からない購入（タイムアウト・5xx）を数える。
// 最終チェックで「課金されたのに注文がない」件数の許容量になる。
func (st *State) unknownPurchaseOutcome() {
	st.payMu.Lock()
	st.unknownOutcome++
	st.payMu.Unlock()
}

func (st *State) priceOf(eventID int64, rank string) (int, bool) {
	v, ok := st.prices.Load(eventID)
	if !ok {
		return 0, false
	}
	p, ok := v.(map[string]int)[rank]
	return p, ok
}

func (st *State) setPrices(eventID int64, ranks []RankJSON) {
	if _, ok := st.prices.Load(eventID); ok {
		return
	}
	m := map[string]int{}
	for _, r := range ranks {
		m[r.Rank] = r.Price
	}
	st.prices.Store(eventID, m)
}
