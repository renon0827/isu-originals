// ISUAUCTION ベンチマーカー。
//
//	bench -target http://127.0.0.1
//
// 結果は標準出力に JSON で出力する。進捗は標準エラー出力。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

type Config struct {
	Target         string
	HostHeader     string
	Duration       time.Duration
	RequestTimeout time.Duration
	InitialFans    int
	MaxFans        int
	Verbose        bool

	// 目玉商品の終了時刻（T0 からの相対）
	HotEndOffset time.Duration
	// 終了から落札確定を確認できるまでの期限
	SettleDeadline time.Duration
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.Target, "target", "http://127.0.0.1", "ベンチマーク対象の URL")
	flag.StringVar(&cfg.HostHeader, "host", "", "Host ヘッダを上書きする場合に指定")
	flag.DurationVar(&cfg.Duration, "duration", 60*time.Second, "負荷走行の時間")
	flag.DurationVar(&cfg.RequestTimeout, "timeout", 10*time.Second, "リクエストのタイムアウト")
	flag.IntVar(&cfg.InitialFans, "initial-fans", 10, "走行開始時のユーザー数")
	flag.IntVar(&cfg.MaxFans, "max-fans", 3000, "ユーザー数の上限")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "毎秒の負荷状況をメッセージに出す")
	flag.Parse()
	cfg.HotEndOffset = 30 * time.Second
	cfg.SettleDeadline = 2 * time.Second

	result := run(cfg)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(result)
	if !result.Pass {
		os.Exit(1)
	}
}

func run(cfg Config) Result {
	st := NewState(cfg)
	ctx := context.Background()
	phase := func(name string) { log.Printf("== %s", name) }

	phase("初期化 (POST /api/initialize)")
	a := NewAgent(cfg.Target, cfg.HostHeader, 30*time.Second)
	res, err := a.Do(ctx, http.MethodPost, "/api/initialize", map[string]string{})
	if err != nil || res.Status != http.StatusOK {
		if err == nil {
			err = fmt.Errorf("status %d: %s", res.Status, truncate(res.Body, 200))
		}
		st.score.Fail("初期化に失敗しました: %v", err)
		return st.score.Result()
	}

	phase("事前チェック")
	if err := validate(ctx, st); err != nil {
		st.score.Fail("事前チェックに失敗しました: %v", err)
		return st.score.Result()
	}
	if failed, _ := st.score.Failed(); failed {
		return st.score.Result()
	}
	// 事前チェック中の商品・入札は採点対象外
	st = NewState(cfg)

	phase("目玉商品の出品")
	st.T0 = time.Now().Add(5 * time.Second).Truncate(time.Second)
	load := &Load{st: st}
	// 落札確定の見届けは走行終了後もしばらく続ける
	settleCtx, cancelSettle := context.WithTimeout(ctx, time.Until(st.T0.Add(cfg.Duration+15*time.Second)))
	defer cancelSettle()
	if err := load.prepareHotItems(ctx, settleCtx); err != nil {
		st.score.Fail("%v", err)
		return st.score.Result()
	}
	time.Sleep(time.Until(st.T0))

	phase(fmt.Sprintf("負荷走行 (%v)", cfg.Duration))
	loadCtx, cancel := context.WithDeadline(ctx, st.T0.Add(cfg.Duration))
	load.Run(loadCtx, settleCtx)
	cancel()

	phase("最終チェック")
	finalCheck(ctx, st, load)

	r := st.score.Result()
	for _, p := range st.score.PenaltySummary() {
		log.Printf("ペナルティ %s", p)
	}
	log.Printf("pass=%v score=%d (手数料 %d円 / 入札 %d / 期限内の落札確定 %d / 遅延 %d / 出品 %d) users=%d left=%d",
		r.Pass, r.Score, r.Fee, r.Bids, r.Settled, r.LateSettled, r.Listed, len(load.fans), load.left.Load())
	return r
}
