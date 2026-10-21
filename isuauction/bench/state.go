package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// BenchItem はベンチマーカーが走行中に出品した商品。入札はすべてベンチマーカーのユーザーが行う。
type BenchItem struct {
	ID          int64
	Seller      *Fan
	StartPrice  int
	Increment   int
	OriginalEnd time.Time
	Hot         bool

	mu sync.Mutex
	// 受理された入札
	bids []AcceptedBid
	// 結果が分からない入札（タイムアウトなど）の数。1 件でもあれば厳密な検証を省く
	unknown int
	// ベンチマーカーが確認できた最新の version とその確認時刻
	knownVersion   int
	knownVersionAt time.Time
	// 見えている最新の終了時刻（延長で伸びる）
	lastEnd time.Time
	// 落札確定を確認したか
	settled    bool
	settledAt  time.Time
	finalPrice *int
	winnerID   int64
	feeCounted bool
	finalEnd   time.Time
}

func (it *BenchItem) recordBid(user int64, max int, r BidResponse) {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.bids = append(it.bids, AcceptedBid{UserID: user, Max: max, At: r.AcceptedAt, BidID: r.BidID})
	if r.Version > it.knownVersion {
		it.knownVersion = r.Version
		it.knownVersionAt = time.Now()
	}
	if r.EndAt.After(it.lastEnd) {
		it.lastEnd = r.EndAt
	}
}

func (it *BenchItem) seeEnd(end time.Time) {
	it.mu.Lock()
	if end.After(it.lastEnd) {
		it.lastEnd = end
	}
	it.mu.Unlock()
}

func (it *BenchItem) end() time.Time {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.lastEnd
}

func (it *BenchItem) isSettled() bool {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.settled
}

// ---- スコア ----

type Score struct {
	mu sync.Mutex

	Fee         int64 // 落札手数料（落札価格の 10%）
	Bids        int64 // 受理された入札
	Settled     int64 // 期限内に落札確定を確認できた商品
	LateSettled int64 // 落札確定が遅れた商品
	Listed      int64 // 出品した商品
	BidRejected int64 // 価格が低すぎる・終了済みなどで拒否された入札
	penalties   map[string]*penaltyAgg
	failed      atomic.Bool
	failReason  atomic.Value
	messages    []string
}

type penaltyAgg struct {
	count  int
	points int
}

func NewScore() *Score { return &Score{penalties: map[string]*penaltyAgg{}} }

func (s *Score) Add(p *int64, n int64) {
	s.mu.Lock()
	*p += n
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

const feeDivisor = 10

type Result struct {
	Pass          bool           `json:"pass"`
	Score         int64          `json:"score"`
	Fee           int64          `json:"fee"`
	Bids          int64          `json:"bids"`
	Settled       int64          `json:"settled"`
	LateSettled   int64          `json:"late_settled"`
	Listed        int64          `json:"listed"`
	BidRejected   int64          `json:"bid_rejected"`
	PenaltyPoints int64          `json:"penalty_points"`
	Penalties     map[string]int `json:"penalties"`
	FailReason    string         `json:"fail_reason,omitempty"`
	Messages      []string       `json:"messages"`
}

func (s *Score) Result() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Result{
		Fee: s.Fee, Bids: s.Bids, Settled: s.Settled, LateSettled: s.LateSettled, Listed: s.Listed, BidRejected: s.BidRejected,
		Penalties: map[string]int{}, Messages: s.messages,
	}
	for reason, a := range s.penalties {
		r.PenaltyPoints += int64(a.points)
		r.Penalties[reason] = a.count
	}
	r.Score = s.Fee/feeDivisor + s.Bids - r.PenaltyPoints
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
	cfg   Config
	score *Score
	stats Stats
	T0    time.Time

	itemsMu sync.Mutex
	items   map[int64]*BenchItem
	order   []*BenchItem

	nextFan atomic.Int64
}

func NewState(cfg Config) *State {
	return &State{cfg: cfg, score: NewScore(), items: map[int64]*BenchItem{}}
}

func (st *State) addItem(it *BenchItem) {
	st.itemsMu.Lock()
	st.items[it.ID] = it
	st.order = append(st.order, it)
	st.itemsMu.Unlock()
}

func (st *State) item(id int64) (*BenchItem, bool) {
	st.itemsMu.Lock()
	defer st.itemsMu.Unlock()
	it, ok := st.items[id]
	return it, ok
}

// openItems はまだ終了していないベンチ商品（終了時刻の早い順）。
func (st *State) openItems(now time.Time) []*BenchItem {
	st.itemsMu.Lock()
	all := append([]*BenchItem(nil), st.order...)
	st.itemsMu.Unlock()
	var out []*BenchItem
	for _, it := range all {
		if it.end().After(now) {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].end().Before(out[j].end()) })
	return out
}

func (st *State) allItems() []*BenchItem {
	st.itemsMu.Lock()
	defer st.itemsMu.Unlock()
	return append([]*BenchItem(nil), st.order...)
}

// checkFreshness は、入札の受理を確認してから 1 秒以上後に始めたリクエストが
// 古い version を返していないか検証する。
func (st *State) checkFreshness(itemID int64, version int, reqStart time.Time) {
	it, ok := st.item(itemID)
	if !ok {
		return
	}
	it.mu.Lock()
	stale := version < it.knownVersion && reqStart.After(it.knownVersionAt.Add(time.Second))
	it.mu.Unlock()
	if stale {
		st.score.Penalize("価格の反映遅延", 5)
	}
}
