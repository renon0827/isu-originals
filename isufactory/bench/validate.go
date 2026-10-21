package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// validate は負荷走行の前に、API が仕様どおりに動くことを少量のリクエストで確認する。
// ここで作ったデータは、走行前の再初期化で消える。
func validate(ctx context.Context, st *State) error {
	newAgent := func() *Agent { return NewAgent(st.cfg.Target, st.cfg.HostHeader, 30*time.Second) }
	call := func(a *Agent, method, path string, body any, want int) (*Response, error) {
		res, err := a.Do(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		if res.Status != want {
			return nil, fmt.Errorf("%s %s: ステータス %d を期待しましたが %d でした: %s", method, path, want, res.Status, truncate(res.Body, 200))
		}
		return res, nil
	}

	// ---- 静的ファイル ----
	top := newAgent()
	res, err := call(top, http.MethodGet, "/", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if !strings.Contains(string(res.Body), "ISUFACTORY") {
		return fmt.Errorf("GET /: トップページの内容が不正です")
	}
	for _, p := range []string{"/assets/app.js", "/assets/app.css"} {
		if _, err := call(top, http.MethodGet, p, nil, http.StatusOK); err != nil {
			return err
		}
	}

	// ---- ログインと権限 ----
	if _, err := call(newAgent(), http.MethodPost, "/api/login", map[string]string{"login_name": operatorLogin(1), "password": "wrong"}, http.StatusUnauthorized); err != nil {
		return err
	}
	if _, err := call(newAgent(), http.MethodGet, "/api/dashboard", nil, http.StatusUnauthorized); err != nil {
		return err
	}
	op := newAgent()
	if err := login(ctx, op, operatorLogin(1)); err != nil {
		return err
	}
	res, err = call(op, http.MethodGet, "/api/me", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if u, err := decode[UserJSON](res); err != nil || u.Role != "operator" {
		return fmt.Errorf("GET /api/me: レスポンスが不正です")
	}
	planner := newAgent()
	if err := login(ctx, planner, plannerLogin(1)); err != nil {
		return err
	}
	if _, err := call(op, http.MethodPost, "/api/plans", nil, http.StatusForbidden); err != nil {
		return err
	}
	if _, err := call(op, http.MethodPost, "/api/orders", map[string]any{"product_id": 1, "quantity": 10, "due_at": time.Now().Add(time.Hour)}, http.StatusForbidden); err != nil {
		return err
	}

	// ---- 製品・部材 ----
	res, err = call(planner, http.MethodGet, "/api/products", nil, http.StatusOK)
	if err != nil {
		return err
	}
	ps, err := decode[struct {
		Products []*ProductJSON `json:"products"`
	}](res)
	if err != nil || len(ps.Products) != 60 {
		return fmt.Errorf("GET /api/products: 製品は 60 件のはずです")
	}
	for _, p := range ps.Products {
		if len(p.BOM) == 0 || p.UnitSeconds <= 0 {
			return fmt.Errorf("GET /api/products: 製品 %d の内容が不正です", p.ID)
		}
	}
	res, err = call(op, http.MethodGet, "/api/products/1/drawing", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if len(res.Body) < 8 || string(res.Body[1:4]) != "PNG" {
		return fmt.Errorf("GET /api/products/1/drawing: PNG ではありません")
	}
	materials := func() (map[int64]int64, error) {
		res, err := call(planner, http.MethodGet, "/api/materials", nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		ms, err := decode[struct {
			Materials []MaterialJSON `json:"materials"`
		}](res)
		if err != nil || len(ms.Materials) != 30 {
			return nil, fmt.Errorf("GET /api/materials: 部材は 30 件のはずです")
		}
		m := map[int64]int64{}
		for _, x := range ms.Materials {
			m[x.ID] = x.Stock
		}
		return m, nil
	}
	stock0, err := materials()
	if err != nil {
		return err
	}

	// ---- センサー送信と異常検知 ----
	m := st.machineList[7]
	dev := newAgent()
	post := func(token string, readings []map[string]any, want int) error {
		res, err := dev.DoHeader(ctx, http.MethodPost, "/api/readings", map[string]any{"readings": readings}, map[string]string{"X-Machine-Token": token})
		if err != nil {
			return err
		}
		if res.Status != want {
			return fmt.Errorf("POST /api/readings: ステータス %d を期待しましたが %d でした", want, res.Status)
		}
		return nil
	}
	now := time.Now().Truncate(time.Microsecond)
	normal := []map[string]any{}
	for i := 0; i < 6; i++ {
		normal = append(normal, map[string]any{"metric": "vibration", "value": 3.0 + float64(i)*0.01, "measured_at": now.Add(time.Duration(i-10) * time.Second)})
	}
	if err := post("invalid-token", normal, http.StatusUnauthorized); err != nil {
		return err
	}
	if err := post(m.Token, []map[string]any{{"metric": "pressure", "value": 1.0, "measured_at": now}}, http.StatusBadRequest); err != nil {
		return err
	}
	if err := post(m.Token, normal, http.StatusCreated); err != nil {
		return err
	}
	spikeAt := now.Add(-2 * time.Second)
	if err := post(m.Token, []map[string]any{{"metric": "vibration", "value": 9.0, "measured_at": spikeAt}}, http.StatusCreated); err != nil {
		return err
	}
	found := false
	for i := 0; i < 6 && !found; i++ {
		res, err := call(op, http.MethodGet, fmt.Sprintf("/api/alerts?machine_id=%d", m.ID), nil, http.StatusOK)
		if err != nil {
			return err
		}
		al, err := decode[struct {
			Alerts []AlertJSON `json:"alerts"`
		}](res)
		if err != nil {
			return fmt.Errorf("GET /api/alerts: %w", err)
		}
		for _, a := range al.Alerts {
			if a.Metric == "vibration" && a.MeasuredAt.Equal(spikeAt) {
				found = true
			}
			if a.MeasuredAt.After(now.Add(-time.Minute)) && !a.MeasuredAt.Equal(spikeAt) {
				return fmt.Errorf("GET /api/alerts: 正常値がアラートになっています")
			}
		}
		if !found {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !found {
		return fmt.Errorf("異常値のアラートが 3 秒以内に作られません")
	}

	// ---- ダッシュボード・設備・時系列 ----
	res, err = call(op, http.MethodGet, "/api/dashboard", nil, http.StatusOK)
	if err != nil {
		return err
	}
	dash, err := decode[DashboardJSON](res)
	if err != nil || len(dash.Lines) != numLines {
		return fmt.Errorf("GET /api/dashboard: ラインは %d 件のはずです", numLines)
	}
	var target *MachineStatusJSON
	for _, l := range dash.Lines {
		if len(l.Machines) != len(machineTypes) {
			return fmt.Errorf("GET /api/dashboard: ライン %d の設備数が不正です", l.ID)
		}
		for _, x := range l.Machines {
			if x.ID == m.ID {
				target = x
			}
		}
	}
	if target == nil || target.Latest["vibration"] == nil || !target.Latest["vibration"].MeasuredAt.Equal(spikeAt) || target.RecentAlerts < 1 {
		return fmt.Errorf("GET /api/dashboard: 設備 %d の最新値・異常件数が不正です", m.ID)
	}
	res, err = call(op, http.MethodGet, fmt.Sprintf("/api/machines/%d/metrics?metric=vibration&minutes=10", m.ID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	mj, err := decode[MetricsJSON](res)
	if err != nil {
		return err
	}
	total, max := 0, 0.0
	for _, p := range mj.Points {
		total += p.Count
		if p.Max > max {
			max = p.Max
		}
	}
	if total != 7 || max != 9.0 {
		return fmt.Errorf("GET /api/machines/%d/metrics: 集計が不正です (件数 %d, 最大 %.2f)", m.ID, total, max)
	}

	// ---- 受注・計画・入荷 ----
	product := ps.Products[4]
	res, err = call(planner, http.MethodPost, "/api/orders", map[string]any{"product_id": product.ID, "quantity": 10, "due_at": time.Now().Add(3 * time.Hour)}, http.StatusCreated)
	if err != nil {
		return err
	}
	order, _ := decode[struct {
		ID int64 `json:"id"`
	}](res)
	planStart := time.Now()
	res, err = call(planner, http.MethodPost, "/api/plans", nil, http.StatusOK)
	if err != nil {
		return err
	}
	pr, err := decode[PlanResponse](res)
	if err != nil || len(pr.Assignments) != 1 || pr.Assignments[0].OrderID != order.ID {
		return fmt.Errorf("POST /api/plans: 登録した受注が計画されません")
	}
	a := pr.Assignments[0]
	if st.machines[a.MachineID] == nil || st.machines[a.MachineID].Type != product.MachineType ||
		a.StartAt.Before(planStart.Add(-time.Second)) || a.EndAt.Sub(a.StartAt) < time.Duration(10*product.UnitSeconds)*time.Second {
		return fmt.Errorf("POST /api/plans: 割当が不正です")
	}
	res, err = call(planner, http.MethodPost, "/api/plans", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if pr2, err := decode[PlanResponse](res); err != nil || len(pr2.Assignments) != 0 {
		return fmt.Errorf("POST /api/plans: 計画済みの受注が再度計画されました")
	}
	stock1, err := materials()
	if err != nil {
		return err
	}
	for _, b := range product.BOM {
		if stock1[b.MaterialID] != stock0[b.MaterialID]-int64(b.Quantity*10) {
			return fmt.Errorf("計画後の部材 %d の在庫が不正です", b.MaterialID)
		}
	}
	res, err = call(op, http.MethodGet, fmt.Sprintf("/api/machines/%d/schedule?hours=48", a.MachineID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	sched, err := decode[ScheduleJSON](res)
	if err != nil {
		return err
	}
	ok := false
	for _, j := range sched.Jobs {
		if j.OrderID == order.ID && j.ProductID == product.ID && absDuration(j.StartAt.Sub(a.StartAt)) <= time.Microsecond {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("GET /api/machines/%d/schedule: 計画した作業が表示されません", a.MachineID)
	}
	mid := product.BOM[0].MaterialID
	if _, err := call(planner, http.MethodPost, fmt.Sprintf("/api/materials/%d/receive", mid), map[string]int{"quantity": 123}, http.StatusOK); err != nil {
		return err
	}
	stock2, err := materials()
	if err != nil {
		return err
	}
	if stock2[mid] != stock1[mid]+123 {
		return fmt.Errorf("入荷後の部材 %d の在庫が不正です", mid)
	}
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
