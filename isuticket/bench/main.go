// ISUTICKET ベンチマーカー。
//
//	bench -target http://127.0.0.1:8080 -payment-url http://<bench の IP>:12346
//
// 結果は標準出力に JSON で出力する。進捗は標準エラー出力。
package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"time"
)

type Config struct {
	Target         string
	HostHeader     string
	PaymentBind    string
	PaymentURL     string
	Duration       time.Duration
	RequestTimeout time.Duration
	InitialFans    int
	MaxFans        int
	Verbose        bool

	// 走行タイムライン（T0 からの相対）
	LotteryEntryEnd time.Duration
	LotteryAnnounce time.Duration
	LotteryDeadline time.Duration
	GateOpenOffset  time.Duration
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.Target, "target", "http://127.0.0.1", "ベンチマーク対象の URL")
	flag.StringVar(&cfg.HostHeader, "host", "", "Host ヘッダを上書きする場合に指定")
	flag.StringVar(&cfg.PaymentBind, "payment-bind", ":12346", "決済モックの待ち受けアドレス")
	flag.StringVar(&cfg.PaymentURL, "payment-url", "http://127.0.0.1:12346", "アプリから見た決済モックの URL")
	flag.DurationVar(&cfg.Duration, "duration", 60*time.Second, "負荷走行の時間")
	flag.DurationVar(&cfg.RequestTimeout, "timeout", 10*time.Second, "リクエストのタイムアウト")
	flag.IntVar(&cfg.InitialFans, "initial-fans", 10, "走行開始時のファン数")
	flag.IntVar(&cfg.MaxFans, "max-fans", 3000, "ファン数の上限")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "毎秒の負荷状況をメッセージに出す")
	flag.Parse()

	cfg.LotteryEntryEnd = 20 * time.Second
	cfg.LotteryAnnounce = 25 * time.Second
	cfg.LotteryDeadline = 3 * time.Second
	cfg.GateOpenOffset = 45 * time.Second

	result := run(cfg)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(result)
	if !result.Pass {
		os.Exit(1)
	}
}

func run(cfg Config) Result {
	payment := NewPaymentServer()
	st := NewState(cfg, payment)

	ln, err := net.Listen("tcp", cfg.PaymentBind)
	if err != nil {
		st.score.Fail("決済モックを起動できません: %v", err)
		return st.score.Result()
	}
	go http.Serve(ln, payment)

	ctx := context.Background()
	phase := func(name string) { log.Printf("== %s", name) }

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
	if failed, _ := st.score.Failed(); failed {
		return st.score.Result()
	}
	// 事前チェック中の決済は採点対象外
	payment.Reset()
	st.score = NewScore()

	phase("走行用公演の作成")
	org := NewAgent(cfg.Target, cfg.HostHeader, 60*time.Second)
	if err := loginOrganizer(ctx, org); err != nil {
		st.score.Fail("%v", err)
		return st.score.Result()
	}
	st.T0 = time.Now().Add(20 * time.Second).Truncate(time.Second)
	prepCtx, cancel := context.WithDeadline(ctx, st.T0.Add(-2*time.Second))
	err = st.prepareEvents(prepCtx, org)
	cancel()
	if err != nil {
		st.score.Fail("走行用公演を作成できません（T0 の 2 秒前までに完了する必要があります）: %v", err)
		return st.score.Result()
	}
	gate, err := NewGate(ctx, st, 8)
	if err != nil {
		st.score.Fail("%v", err)
		return st.score.Result()
	}
	time.Sleep(time.Until(st.T0))

	phase(fmt.Sprintf("負荷走行 (%v)", cfg.Duration))
	loadCtx, cancel := context.WithTimeout(ctx, cfg.Duration)
	load := &Load{st: st, gate: gate, org: org}
	load.Run(loadCtx)
	cancel()

	phase("最終チェック")
	finalCheck(ctx, st, load)

	r := st.score.Result()
	for _, p := range st.score.PenaltySummary() {
		log.Printf("ペナルティ %s", p)
	}
	log.Printf("pass=%v score=%d (先着 %d円 / 抽選 %d円 / リセール手数料 %d円 / 入場 %d) fans=%d left=%d",
		r.Pass, r.Score, r.RevenueFirstCome, r.RevenueLottery, r.ResaleFee, r.Entries, len(load.fans), load.left.Load())
	return r
}

func initialize(ctx context.Context, st *State) error {
	a := NewAgent(st.cfg.Target, st.cfg.HostHeader, 30*time.Second)
	res, err := a.Do(ctx, http.MethodPost, "/api/initialize", map[string]string{"payment_url": st.cfg.PaymentURL})
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return fmt.Errorf("status %d: %s", res.Status, truncate(res.Body, 200))
	}

	res, err = a.Do(ctx, http.MethodGet, "/api/public-key", nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return fmt.Errorf("GET /api/public-key: status %d", res.Status)
	}
	block, _ := pem.Decode(res.Body)
	if block == nil {
		return errors.New("GET /api/public-key: PEM ではありません")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("GET /api/public-key: %w", err)
	}
	pk, ok := k.(*rsa.PublicKey)
	if !ok || pk.N.BitLen() != 2048 {
		return errors.New("GET /api/public-key: RSA-2048 の公開鍵ではありません")
	}
	st.pubKey = pk
	return nil
}

func makeJPEG(seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	c := color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255}
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{c.R ^ uint8(x/40), c.G ^ uint8(y/40), c.B, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}
