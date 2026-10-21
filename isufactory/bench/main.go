// ISUFACTORY ベンチマーカー。
//
//	bench -target http://127.0.0.1
//
// 結果は標準出力に JSON で出力する。進捗は標準エラー出力。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Target         string
	HostHeader     string
	Duration       time.Duration
	RequestTimeout time.Duration
	InitialOps     int
	MaxOps         int
	Verbose        bool
	AlertDeadline  time.Duration
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.Target, "target", "http://127.0.0.1", "ベンチマーク対象の URL")
	flag.StringVar(&cfg.HostHeader, "host", "", "Host ヘッダを上書きする場合に指定")
	flag.DurationVar(&cfg.Duration, "duration", 60*time.Second, "負荷走行の時間")
	flag.DurationVar(&cfg.RequestTimeout, "timeout", 10*time.Second, "リクエストのタイムアウト")
	flag.IntVar(&cfg.InitialOps, "initial-operators", 2, "走行開始時の現場担当の人数")
	flag.IntVar(&cfg.MaxOps, "max-operators", 100, "現場担当の人数の上限")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "毎秒の負荷状況をメッセージに出す")
	flag.Parse()
	cfg.AlertDeadline = 3 * time.Second

	result := run(cfg)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(result)
	if !result.Pass {
		os.Exit(1)
	}
}

// ---- 直近 1 秒のリクエスト統計（現場担当の追加判断に使う） ----

type Stats struct {
	requests atomic.Int64
	errors   atomic.Int64
	slow     atomic.Int64
}

func (s *Stats) record(res *Response, err error) {
	s.requests.Add(1)
	if err != nil || (res != nil && res.Status >= 500) {
		s.errors.Add(1)
		return
	}
	if res != nil && !res.FromCache && res.Latency > time.Second {
		s.slow.Add(1)
	}
}

func (s *Stats) swap() (req, errs, slow int64) {
	return s.requests.Swap(0), s.errors.Swap(0), s.slow.Swap(0)
}

// ---- 全体の状態 ----

type State struct {
	cfg   Config
	score *Score
	stats Stats
	T0    time.Time

	machines    map[int64]*MachineState
	machineList []*MachineState
	products    map[int64]*ProductJSON
	productList []*ProductJSON

	matMu         sync.Mutex
	initialStock  map[int64]int64
	receivedSent  map[int64]int64 // 送った入荷（結果不明を含む）
	receivedAcked map[int64]int64 // 受理が確定した入荷
	consumed      map[int64]int64

	orderMu     sync.Mutex
	orders      map[int64]*OrderState
	assignments []*PlannedJSON
	// 作成レスポンスより先に計画に現れた受注（最終チェックで照合する）
	earlyPlanned []PlannedJSON

	// 直近の計画 API の応答時間（ミリ秒）。受注の流入ペースの調整に使う
	lastPlanMillis atomic.Int64
}

func newState(cfg Config) *State {
	return &State{
		cfg: cfg, score: NewScore(),
		machines: map[int64]*MachineState{}, products: map[int64]*ProductJSON{},
		initialStock: map[int64]int64{}, receivedSent: map[int64]int64{}, receivedAcked: map[int64]int64{},
		consumed: map[int64]int64{}, orders: map[int64]*OrderState{},
	}
}

func (st *State) newAgent() *Agent {
	return NewAgent(st.cfg.Target, st.cfg.HostHeader, st.cfg.RequestTimeout)
}

func run(cfg Config) Result {
	ctx := context.Background()
	st := newState(cfg)
	phase := func(name string) { log.Printf("== %s", name) }

	for l := 1; l <= numLines; l++ {
		for ti, mt := range machineTypes {
			id := int64((l-1)*len(machineTypes) + ti + 1)
			m := &MachineState{ID: id, Type: mt, Token: machineToken(id)}
			st.machines[id] = m
			st.machineList = append(st.machineList, m)
		}
	}

	phase("初期化 (POST /api/initialize)")
	if err := initialize(ctx, st); err != nil {
		st.score.Fail("初期化に失敗しました: %v", err)
		return st.score.Result()
	}

	phase("事前チェック")
	if err := validate(ctx, st); err != nil {
		st.score.Fail("事前チェックに失敗しました: %v", err)
		return st.score.Result()
	}

	// 事前チェックで変わったデータを戻してから走行する
	phase("再初期化")
	if err := initialize(ctx, st); err != nil {
		st.score.Fail("初期化に失敗しました: %v", err)
		return st.score.Result()
	}
	if err := st.loadBaseline(ctx); err != nil {
		st.score.Fail("初期状態を取得できません: %v", err)
		return st.score.Result()
	}
	st.score = NewScore()

	phase("負荷走行 (" + cfg.Duration.String() + ")")
	st.T0 = time.Now()
	loadCtx, cancel := context.WithTimeout(ctx, cfg.Duration)
	load := &Load{st: st}
	load.Run(loadCtx)
	cancel()

	phase("最終チェック")
	finalCheck(ctx, st)

	r := st.score.Result()
	for _, p := range st.score.PenaltySummary() {
		log.Printf("ペナルティ %s", p)
	}
	log.Printf("pass=%v score=%d (計測値 %d 件=%d点 / 閲覧 %d点 / 計画 %d点: 納期内 %d・遅れ %d) operators=%d left=%d",
		r.Pass, r.Score, r.Readings, r.ReadingPoints, r.ViewPoints, r.PlanPoints, r.OrdersOnTime, r.OrdersLate,
		load.opsTotal.Load(), load.opsLeft.Load())
	return r
}

func initialize(ctx context.Context, st *State) error {
	a := NewAgent(st.cfg.Target, st.cfg.HostHeader, 30*time.Second)
	res, err := a.Do(ctx, http.MethodPost, "/api/initialize", map[string]string{})
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return errStatus("POST /api/initialize", res)
	}
	return nil
}

// loadBaseline は走行前の製品・部材在庫を取得する。
func (st *State) loadBaseline(ctx context.Context) error {
	a := st.newAgent()
	if err := login(ctx, a, plannerLogin(1)); err != nil {
		return err
	}
	res, err := a.Do(ctx, http.MethodGet, "/api/products", nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return errStatus("GET /api/products", res)
	}
	ps, err := decode[struct {
		Products []*ProductJSON `json:"products"`
	}](res)
	if err != nil {
		return err
	}
	st.products = map[int64]*ProductJSON{}
	st.productList = ps.Products
	for _, p := range ps.Products {
		st.products[p.ID] = p
	}
	res, err = a.Do(ctx, http.MethodGet, "/api/materials", nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return errStatus("GET /api/materials", res)
	}
	ms, err := decode[struct {
		Materials []MaterialJSON `json:"materials"`
	}](res)
	if err != nil {
		return err
	}
	for _, m := range ms.Materials {
		st.initialStock[m.ID] = m.Stock
	}
	return nil
}

func login(ctx context.Context, a *Agent, loginName string) error {
	res, err := a.Do(ctx, http.MethodPost, "/api/login", map[string]string{"login_name": loginName, "password": passwordOf(loginName)})
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return errStatus("POST /api/login", res)
	}
	return nil
}
