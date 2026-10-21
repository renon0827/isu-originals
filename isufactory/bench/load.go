package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

func errStatus(what string, res *Response) error {
	return fmt.Errorf("%s: status %d: %s", what, res.Status, truncate(res.Body, 200))
}

type Load struct {
	st *State
	wg sync.WaitGroup

	opsActive atomic.Int64
	opsTotal  atomic.Int64
	opsLeft   atomic.Int64

	spikes chan spikeCheck

	// 受注の流入ペース（件/秒）
	orderRate atomic.Int64

	// 現場担当の処理中リクエストの開始時刻（遅いリクエストが溜まっていたら人数を増やさない）
	reqSeq   atomic.Int64
	inflight sync.Map
}

// opCall は現場担当のリクエスト。処理中の間は inflight に登録しておく。
func (l *Load) opCall(ctx context.Context, a *Agent, method, path string) (*Response, error) {
	id := l.reqSeq.Add(1)
	l.inflight.Store(id, time.Now())
	defer l.inflight.Delete(id)
	return l.call(ctx, a, method, path, nil)
}

// hasStuckRequests は 1 秒以上処理中の現場担当リクエストがあるか。
func (l *Load) hasStuckRequests() bool {
	stuck := false
	l.inflight.Range(func(_, v any) bool {
		if time.Since(v.(time.Time)) > time.Second {
			stuck = true
			return false
		}
		return true
	})
	return stuck
}

type spikeCheck struct {
	m       *MachineState
	metric  string
	at      time.Time
	ackedAt time.Time
}

func (l *Load) Run(ctx context.Context) {
	st := l.st
	l.spikes = make(chan spikeCheck, 10000)
	l.orderRate.Store(2)

	for _, m := range st.machineList {
		l.wg.Add(1)
		go func(m *MachineState) {
			defer l.wg.Done()
			l.runDevice(ctx, m)
		}(m)
	}
	for i := 0; i < 4; i++ {
		l.wg.Add(1)
		go func(i int) {
			defer l.wg.Done()
			l.runAlertChecker(ctx, i)
		}(i)
	}
	l.wg.Add(3)
	go func() { defer l.wg.Done(); l.runSales(ctx) }()
	go func() { defer l.wg.Done(); l.runReceiving(ctx) }()
	go func() { defer l.wg.Done(); l.runPlanner(ctx, 2) }()
	l.wg.Add(1)
	go func() { defer l.wg.Done(); l.runPlanner(ctx, 3) }()

	l.spawnOperators(ctx, st.cfg.InitialOps)

	// 1 秒ごとに直近の応答状況を見て現場担当を増やす
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return
		case <-ticker.C:
		}
		if failed, _ := st.score.Failed(); failed {
			continue
		}
		req, errs, slow := st.stats.swap()
		active := l.opsActive.Load()
		add := 0
		switch {
		case req == 0 || l.hasStuckRequests():
		case errs == 0 && slow == 0:
			add = int(active / 8)
			if add < 2 {
				add = 2
			}
		case errs == 0 && slow*50 <= req:
			add = 1
		}
		if int(active)+add > st.cfg.MaxOps {
			add = st.cfg.MaxOps - int(active)
		}
		if add > 0 {
			l.spawnOperators(ctx, add)
		}
		if st.cfg.Verbose {
			st.score.Message(fmt.Sprintf("operators=%d (+%d, left=%d) orders/s=%d req=%d err=%d slow=%d",
				active, add, l.opsLeft.Load(), l.orderRate.Load(), req, errs, slow))
		}
	}
}

// call は API を呼び、タイムアウト・5xx をペナルティとして記録する。
func (l *Load) call(ctx context.Context, a *Agent, method, path string, body any) (*Response, error) {
	res, err := a.Do(ctx, method, path, body)
	l.st.stats.record(res, err)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrTimeout) {
			l.st.score.Penalize("タイムアウト", 5)
		} else {
			l.st.score.Penalize("通信エラー", 5)
		}
		return nil, err
	}
	if res.Status >= 500 {
		l.st.score.Penalize("サーバーエラー (5xx)", 5)
	}
	return res, nil
}

func (l *Load) unexpected(path string, res *Response) {
	if res.Status < 500 {
		l.st.score.Penalize("想定外のステータスコード", 10)
		l.st.score.Message(fmt.Sprintf("%s: 想定外のステータス %d: %s", path, res.Status, truncate(res.Body, 200)))
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ---- センサー送信端末 ----

func (l *Load) runDevice(ctx context.Context, m *MachineState) {
	st := l.st
	a := st.newAgent()
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + m.ID))
	base := map[string]float64{}
	for k, v := range metricBase {
		base[k] = v * (0.9 + 0.02*float64(m.ID%10))
	}
	seq := 0
	// 送信間隔は応答の速さに合わせて伸び縮みする（速いほど多く送る）
	interval := 3 * time.Second
	for ctx.Err() == nil {
		now := time.Now().Truncate(time.Microsecond)
		batch := make([]sentReading, 10)
		spikeAt := -1
		if rng.Intn(15) == 0 {
			spikeAt = rng.Intn(10)
		}
		for i := range batch {
			metric := metricNames[(seq+i)%len(metricNames)]
			r := sentReading{metric: metric, measuredAt: now.Add(time.Duration(i) * time.Millisecond),
				value: base[metric] * (1 + (rng.Float64()-0.5)*0.1)}
			// 異常値は、判定に十分な通常値が溜まっている指標にだけ混ぜる
			if i == spikeAt && m.recentNormals(metric, now) >= 8 {
				r.value = base[metric] * 2.2
				r.spike = true
			}
			batch[i] = r
		}
		seq += len(batch)
		body := make([]map[string]any, len(batch))
		for i, r := range batch {
			body[i] = map[string]any{"metric": r.metric, "value": r.value, "measured_at": r.measuredAt}
		}
		res, err := l.postReadings(ctx, a, m.Token, body)
		switch {
		case err != nil || res.Status >= 500:
			m.addUnknown(batch)
		case res.Status == http.StatusCreated:
			ackedAt := time.Now()
			m.addAcked(batch, ackedAt)
			st.score.AddReadings(len(batch))
			for _, r := range batch {
				if r.spike {
					select {
					case l.spikes <- spikeCheck{m: m, metric: r.metric, at: r.measuredAt, ackedAt: ackedAt}:
					default:
					}
				}
			}
		default:
			m.addUnknown(batch)
			l.unexpected("POST /api/readings", res)
		}
		switch {
		case err != nil || res.Status != http.StatusCreated || res.Latency > 300*time.Millisecond:
			interval = min(interval*3/2, 10*time.Second)
		case res.Latency < 100*time.Millisecond:
			interval = max(interval*4/5, 50*time.Millisecond)
		}
		if !sleepCtx(ctx, interval) {
			return
		}
	}
}

func (l *Load) postReadings(ctx context.Context, a *Agent, token string, body any) (*Response, error) {
	res, err := a.DoHeader(ctx, http.MethodPost, "/api/readings", map[string]any{"readings": body}, map[string]string{"X-Machine-Token": token})
	l.st.stats.record(res, err)
	if err != nil {
		if ctx.Err() == nil {
			if errors.Is(err, ErrTimeout) {
				l.st.score.Penalize("タイムアウト", 5)
			} else {
				l.st.score.Penalize("通信エラー", 5)
			}
		}
		return nil, err
	}
	if res.Status >= 500 {
		l.st.score.Penalize("サーバーエラー (5xx)", 5)
	}
	return res, nil
}

// ---- 異常検知の確認 ----

func (l *Load) runAlertChecker(ctx context.Context, i int) {
	st := l.st
	a := st.newAgent()
	if err := login(ctx, a, operatorLogin(41+i)); err != nil {
		st.score.Fail("アラート確認用にログインできません: %v", err)
		return
	}
	seenFalse := map[int64]bool{}
	for {
		var sc spikeCheck
		select {
		case <-ctx.Done():
			return
		case sc = <-l.spikes:
		}
		if !sleepCtx(ctx, time.Until(sc.ackedAt.Add(st.cfg.AlertDeadline))) {
			return
		}
		path := fmt.Sprintf("/api/alerts?machine_id=%d&limit=100", sc.m.ID)
		res, err := l.call(ctx, a, http.MethodGet, path, nil)
		if err != nil {
			continue
		}
		if res.Status != http.StatusOK {
			l.unexpected(path, res)
			continue
		}
		body, err := decode[struct {
			Alerts []AlertJSON `json:"alerts"`
		}](res)
		if err != nil {
			st.score.Penalize("不正なレスポンス", 10)
			continue
		}
		found := false
		for _, al := range body.Alerts {
			if al.MachineID != sc.m.ID {
				st.score.Fail("machine_id=%d で絞り込んだアラートに設備 %d のものが含まれています", sc.m.ID, al.MachineID)
				return
			}
			if al.Metric == sc.metric && al.MeasuredAt.Equal(sc.at) {
				found = true
			}
			if al.MeasuredAt.After(st.T0) && !seenFalse[al.ID] && !sc.m.isKnownSpike(al.Metric, al.MeasuredAt) {
				seenFalse[al.ID] = true
				st.score.Penalize("異常の誤検知", 50)
			}
		}
		if !found {
			st.score.Penalize("異常の見逃し（3 秒以内に検知されない）", 50)
		}
	}
}

// ---- 現場担当（閲覧） ----

func (l *Load) spawnOperators(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		idx := int(l.opsTotal.Add(1)-1)%(numOperators-10) + 1
		l.opsActive.Add(1)
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer l.opsActive.Add(-1)
			if left := l.runOperator(ctx, operatorLogin(idx)); left {
				l.opsLeft.Add(1)
			}
		}()
	}
}

// runOperator は 1 人の現場担当として閲覧を繰り返す。不満がたまると帰る（true を返す）。
func (l *Load) runOperator(ctx context.Context, loginName string) bool {
	st := l.st
	a := st.newAgent()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	if err := login(ctx, a, loginName); err != nil {
		return false
	}
	strikes := 0
	strike := func(res *Response, limit time.Duration) {
		if res == nil || res.Latency > limit {
			strikes++
		} else if strikes > 0 {
			strikes--
		}
	}
	staticDone := false
	for ctx.Err() == nil && strikes < 3 {
		if failed, _ := st.score.Failed(); failed {
			return false
		}
		if !staticDone {
			for _, p := range []string{"/", "/assets/app.js", "/assets/app.css"} {
				res, err := a.GetStatic(ctx, p)
				st.stats.record(res, err)
				if err != nil || res.Status != 200 {
					st.score.Penalize("静的ファイルの取得失敗", 5)
				}
			}
			staticDone = true
		}

		// ダッシュボード
		reqStart := time.Now()
		res, err := l.opCall(ctx, a, http.MethodGet, "/api/dashboard")
		strike(res, 2*time.Second)
		if err != nil {
			continue
		}
		if res.Status != http.StatusOK {
			l.unexpected("GET /api/dashboard", res)
			continue
		}
		dash, err := decode[DashboardJSON](res)
		if err != nil {
			st.score.Penalize("不正なレスポンス", 10)
			continue
		}
		if !l.checkDashboard(&dash, reqStart) {
			continue
		}
		st.score.AddView(pointsDashboard)

		// 設備を 1 台選んで詳細を見る
		m := st.machineList[rng.Intn(len(st.machineList))]
		if !l.viewMachine(ctx, a, m, rng, strike) {
			continue
		}
		// 人が画面を見て考える時間
		if !sleepCtx(ctx, time.Duration(500+rng.Intn(1000))*time.Millisecond) {
			break
		}
	}
	return strikes >= 3
}

// checkLatest は最新値が 2 秒以上古くないか検証する。
func (l *Load) checkLatest(m *MachineState, latest map[string]*LatestJSON, reqStart time.Time) bool {
	want := m.latestAckedBefore(reqStart.Add(-2 * time.Second))
	for metric, t := range want {
		got := latest[metric]
		if got == nil || got.MeasuredAt.Before(t) {
			l.st.score.Penalize("最新値が古い", 10)
			return false
		}
	}
	return true
}

func (l *Load) checkDashboard(d *DashboardJSON, reqStart time.Time) bool {
	n := 0
	for _, line := range d.Lines {
		for _, ms := range line.Machines {
			n++
			m, ok := l.st.machines[ms.ID]
			if !ok || ms.MachineType != m.Type {
				l.st.score.Fail("ダッシュボードの設備 %d の情報が不正です", ms.ID)
				return false
			}
			if !l.checkLatest(m, ms.Latest, reqStart) {
				return false
			}
		}
	}
	if n != len(l.st.machineList) {
		l.st.score.Fail("ダッシュボードの設備数が不正です (%d)", n)
		return false
	}
	return true
}

func (l *Load) viewMachine(ctx context.Context, a *Agent, m *MachineState, rng *rand.Rand, strike func(*Response, time.Duration)) bool {
	st := l.st
	reqStart := time.Now()
	path := fmt.Sprintf("/api/machines/%d", m.ID)
	res, err := l.opCall(ctx, a, http.MethodGet, path)
	strike(res, 2*time.Second)
	if err != nil {
		return false
	}
	if res.Status != http.StatusOK {
		l.unexpected(path, res)
		return false
	}
	ms, err := decode[MachineStatusJSON](res)
	if err != nil {
		st.score.Penalize("不正なレスポンス", 10)
		return false
	}
	if !l.checkLatest(m, ms.Latest, reqStart) {
		return false
	}
	st.score.AddView(pointsMachine)

	// 時系列（直近 10 分）
	metric := metricNames[rng.Intn(len(metricNames))]
	reqStart = time.Now()
	path = fmt.Sprintf("/api/machines/%d/metrics?metric=%s&minutes=10", m.ID, metric)
	res, err = l.opCall(ctx, a, http.MethodGet, path)
	strike(res, 2*time.Second)
	if err != nil {
		return false
	}
	if res.Status != http.StatusOK {
		l.unexpected(path, res)
		return false
	}
	mj, err := decode[MetricsJSON](res)
	if err != nil {
		st.score.Penalize("不正なレスポンス", 10)
		return false
	}
	// 窓の端（分の切り捨て）を避け、確実に含まれる範囲の件数・最大値を検証する
	from := reqStart.Add(-9 * time.Minute)
	wantN, wantMax := m.windowStats(metric, from, reqStart.Add(-2*time.Second), reqStart.Add(-2*time.Second))
	// アプリの集計窓は [now-10分 を分で切り捨て, now] なので、[reqStart-9分, reqStart-2秒) を必ず含む
	gotN, gotMax := 0, 0.0
	for _, p := range mj.Points {
		if p.Minute.Before(from.Truncate(time.Minute)) {
			continue
		}
		gotN += p.Count
		if p.Max > gotMax {
			gotMax = p.Max
		}
	}
	if gotN < wantN || gotMax+1e-6 < wantMax {
		st.score.Penalize("時系列の集計が古い・不正", 10)
		return false
	}
	st.score.AddView(pointsMachine)

	// 作業予定と図面
	path = fmt.Sprintf("/api/machines/%d/schedule?hours=12", m.ID)
	res, err = l.opCall(ctx, a, http.MethodGet, path)
	strike(res, 2*time.Second)
	if err != nil {
		return false
	}
	if res.Status != http.StatusOK {
		l.unexpected(path, res)
		return false
	}
	sched, err := decode[ScheduleJSON](res)
	if err != nil {
		st.score.Penalize("不正なレスポンス", 10)
		return false
	}
	st.score.AddView(pointsMachine)
	if len(sched.Jobs) > 0 {
		j := sched.Jobs[0]
		res, err := a.GetStatic(ctx, fmt.Sprintf("/api/products/%d/drawing", j.ProductID))
		st.stats.record(res, err)
		if err != nil || res.Status != 200 || len(res.Body) < 8 || string(res.Body[1:4]) != "PNG" {
			if ctx.Err() == nil {
				st.score.Penalize("図面の取得失敗", 5)
			}
		}
	}
	return true
}

// ---- 計画担当 ----

// runSales は受注を登録し続ける。計画 API が速く返るほど受注が増える。
func (l *Load) runSales(ctx context.Context) {
	st := l.st
	a := st.newAgent()
	if err := login(ctx, a, plannerLogin(1)); err != nil {
		st.score.Fail("計画担当でログインできません: %v", err)
		return
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	last := time.Now()
	for ctx.Err() == nil {
		if time.Since(last) >= 3*time.Second {
			last = time.Now()
			ms := st.lastPlanMillis.Load()
			rate := l.orderRate.Load()
			if ms > 0 && ms < 1500 && rate < 40 {
				l.orderRate.Add(1)
			} else if ms > 5000 && rate > 1 {
				l.orderRate.Add(-1)
			}
		}
		rate := l.orderRate.Load()
		p := st.productList[rng.Intn(len(st.productList))]
		qty := 10 + rng.Intn(51)
		due := time.Now().Add(time.Duration(60+rng.Intn(241)) * time.Minute).Truncate(time.Second)
		res, err := l.call(ctx, a, http.MethodPost, "/api/orders", map[string]any{"product_id": p.ID, "quantity": qty, "due_at": due})
		if err == nil {
			if res.Status == http.StatusCreated {
				body, err := decode[struct {
					ID int64 `json:"id"`
				}](res)
				if err == nil && body.ID > 0 {
					st.orderMu.Lock()
					st.orders[body.ID] = &OrderState{ID: body.ID, Product: p, Quantity: qty, DueAt: due, CreatedAt: time.Now()}
					st.orderMu.Unlock()
				}
			} else {
				l.unexpected("POST /api/orders", res)
			}
		}
		if !sleepCtx(ctx, time.Second/time.Duration(rate)) {
			return
		}
	}
}

// runReceiving は部材の入荷を定期的に登録する。
func (l *Load) runReceiving(ctx context.Context) {
	st := l.st
	a := st.newAgent()
	if err := login(ctx, a, plannerLogin(4)); err != nil {
		st.score.Fail("計画担当でログインできません: %v", err)
		return
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	ids := make([]int64, 0, len(st.initialStock))
	for id := range st.initialStock {
		ids = append(ids, id)
	}
	for ctx.Err() == nil {
		id := ids[rng.Intn(len(ids))]
		qty := 1000 + rng.Intn(2001)
		st.matMu.Lock()
		st.receivedSent[id] += int64(qty)
		st.matMu.Unlock()
		path := fmt.Sprintf("/api/materials/%d/receive", id)
		res, err := l.call(ctx, a, http.MethodPost, path, map[string]int{"quantity": qty})
		if err == nil {
			if res.Status == http.StatusOK {
				st.matMu.Lock()
				st.receivedAcked[id] += int64(qty)
				st.matMu.Unlock()
			} else {
				l.unexpected(path, res)
			}
		}
		if !sleepCtx(ctx, 700*time.Millisecond) {
			return
		}
	}
}

// runPlanner は計画 API を呼び続け、返ってきた割当を検証・採点する。
func (l *Load) runPlanner(ctx context.Context, idx int) {
	st := l.st
	a := st.newAgent()
	if err := login(ctx, a, plannerLogin(idx)); err != nil {
		st.score.Fail("計画担当でログインできません: %v", err)
		return
	}
	for ctx.Err() == nil {
		if failed, _ := st.score.Failed(); failed {
			return
		}
		reqStart := time.Now()
		res, err := l.call(ctx, a, http.MethodPost, "/api/plans", nil)
		if err == nil {
			if res.Status == http.StatusOK {
				st.lastPlanMillis.Store(res.Latency.Milliseconds())
				pr, err := decode[PlanResponse](res)
				if err != nil {
					st.score.Penalize("不正なレスポンス", 10)
				} else {
					l.checkPlan(&pr, reqStart)
				}
			} else {
				l.unexpected("POST /api/plans", res)
			}
		}
		if !sleepCtx(ctx, 500*time.Millisecond) {
			return
		}
	}
}

func (l *Load) checkPlan(pr *PlanResponse, reqStart time.Time) {
	st := l.st
	for i := range pr.Assignments {
		p := pr.Assignments[i]
		st.orderMu.Lock()
		o, ok := st.orders[p.OrderID]
		if !ok {
			// 受注作成のレスポンスより先に計画された可能性がある。最終チェックで照合する
			st.earlyPlanned = append(st.earlyPlanned, p)
			st.orderMu.Unlock()
			continue
		}
		if o.Planned != nil {
			st.orderMu.Unlock()
			st.score.Fail("受注 %d が二重に計画されました", p.OrderID)
			return
		}
		o.Planned = &p
		st.assignments = append(st.assignments, &p)
		st.orderMu.Unlock()
		if !l.checkAssignment(o, &p, reqStart) {
			return
		}
	}
}

// checkAssignment は 1 件の割当の妥当性を検証し、部材を消費して採点する。
func (l *Load) checkAssignment(o *OrderState, p *PlannedJSON, reqStart time.Time) bool {
	st := l.st
	m, ok := st.machines[p.MachineID]
	if !ok || m.Type != o.Product.MachineType {
		st.score.Fail("受注 %d が対応していない設備 %d に割り当てられました", o.ID, p.MachineID)
		return false
	}
	if p.StartAt.Before(reqStart.Add(-time.Second)) || p.StartAt.After(reqStart.Add(48*time.Hour)) {
		st.score.Fail("受注 %d の開始時刻が計画時点より過去、または 48 時間より先です", o.ID)
		return false
	}
	work := time.Duration(o.Quantity*o.Product.UnitSeconds) * time.Second
	if p.EndAt.Sub(p.StartAt) < work-time.Millisecond {
		st.score.Fail("受注 %d の作業時間が短すぎます (%v < %v)", o.ID, p.EndAt.Sub(p.StartAt), work)
		return false
	}
	st.matMu.Lock()
	for _, b := range o.Product.BOM {
		st.consumed[b.MaterialID] += int64(b.Quantity * o.Quantity)
		if st.consumed[b.MaterialID] > st.initialStock[b.MaterialID]+st.receivedSent[b.MaterialID] {
			st.matMu.Unlock()
			st.score.Fail("部材 %d が在庫を超えて引き当てられました", b.MaterialID)
			return false
		}
	}
	st.matMu.Unlock()
	st.score.AddPlan(p.EndAt.Sub(o.DueAt))
	return true
}
