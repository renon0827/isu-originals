// ISUAUCTION の初期データ生成器。
// 固定 seed で決定的に SQL を標準出力へ書き出す。
//
//	go run . > initial.sql && mysql isuauction < initial.sql
//
// ベンチマーカーは以下の規則を前提にしている（変更時は bench 側も合わせる）:
//   - ユーザー: login_name = user%05d (1..NumUsers)、password = "isu-" + login_name
//   - 商品 ID 1..NumEnded は終了済み、NumEnded+1..NumEnded+NumOpen は出品中（終了は遠い未来）
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	NumUsers   = 30000
	NumImages  = 300
	NumEnded   = 120000
	NumOpen    = 3000
	NumWatches = 300000
	NumNotices = 200000
	batchSize  = 1000
	timeLayout = "2006-01-02 15:04:05.000000"
)

var (
	rng = rand.New(rand.NewSource(20261021))
	out *bufio.Writer

	farFuture = time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC)
)

func main() {
	out = bufio.NewWriterSize(os.Stdout, 1<<20)
	defer out.Flush()

	fmt.Fprintln(out, "SET NAMES utf8mb4;")
	fmt.Fprintln(out, "SET autocommit=0; SET unique_checks=0; SET foreign_key_checks=0;")
	genUsers()
	genCategories()
	genImages()
	genItems()
	genWatches()
	genNotifications()
	fmt.Fprintln(out, "COMMIT;")
}

// ---- SQL 出力ヘルパー ----

func ts(t time.Time) string { return "'" + t.UTC().Format(timeLayout) + "'" }

func q(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func d(v int64) string { return fmt.Sprint(v) }

func randHex(n int) string {
	b := make([]byte, n/2)
	rng.Read(b)
	return hex.EncodeToString(b)
}

type batcher struct {
	head string
	size int
	rows []string
}

func newBatcher(table, cols string) *batcher {
	return &batcher{head: fmt.Sprintf("INSERT INTO `%s` (%s) VALUES ", table, cols), size: batchSize}
}

func (b *batcher) add(vals ...string) {
	b.rows = append(b.rows, "("+strings.Join(vals, ",")+")")
	if len(b.rows) >= b.size {
		b.flush()
	}
}

func (b *batcher) flush() {
	if len(b.rows) == 0 {
		return
	}
	out.WriteString(b.head)
	out.WriteString(strings.Join(b.rows, ","))
	out.WriteString(";\n")
	b.rows = b.rows[:0]
}

// ---- users ----

var nickA = []string{"椅子", "空気", "回転", "背もたれ", "座布団", "折りたたみ", "ゲーミング", "ロッキング", "パイプ", "スツール"}
var nickB = []string{"マニア", "コレクター", "職人", "愛好家", "の民", "研究家", "おじさん", "女王", "王子", "ハンター"}

func genUsers() {
	b := newBatcher("users", "id,login_name,nickname,password_hash,salt,created_at")
	created := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= NumUsers; i++ {
		login := fmt.Sprintf("user%05d", i)
		salt := randHex(16)
		h := sha256.Sum256([]byte(salt + "isu-" + login))
		nick := fmt.Sprintf("%s%s%d", nickA[rng.Intn(len(nickA))], nickB[rng.Intn(len(nickB))], rng.Intn(1000))
		b.add(d(int64(i)), q(login), q(nick), q(hex.EncodeToString(h[:])), q(salt), ts(created.Add(time.Duration(i)*time.Minute)))
	}
	b.flush()
}

// ---- categories ----

var categories = []string{
	"オフィスチェア", "ダイニングチェア", "ソファ", "スツール", "座椅子", "ベンチ", "ゲーミングチェア", "ロッキングチェア",
	"子ども用いす", "アウトドアチェア", "アンティーク", "デザイナーズ", "パーツ・部品", "クッション", "フィギュア", "その他",
}

func genCategories() {
	b := newBatcher("categories", "id,name")
	for i, c := range categories {
		b.add(d(int64(i+1)), q(c))
	}
	b.flush()
}

// ---- images ----

func genImages() {
	b := newBatcher("images", "id,data")
	b.size = 20
	for i := 1; i <= NumImages; i++ {
		b.add(d(int64(i)), "0x"+hex.EncodeToString(makeJPEG(int64(i))))
	}
	b.flush()
}

// makeJPEG は商品ごとの決定的な写真風画像（JPEG, 480x360）を作る。
func makeJPEG(seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	const w, h = 480, 360
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{uint8(150 + r.Intn(100)), uint8(150 + r.Intn(100)), uint8(150 + r.Intn(100)), 255}
	fg := color.RGBA{uint8(r.Intn(160)), uint8(r.Intn(160)), uint8(r.Intn(160)), 255}
	// 椅子っぽいシルエット（座面・背もたれ・脚）
	seatY := 180 + r.Intn(40)
	left, right := 140+r.Intn(30), 320+r.Intn(30)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := bg
			shade := uint8((x + y) / 12)
			c.R, c.G, c.B = c.R-shade/3, c.G-shade/3, c.B-shade/3
			inSeat := y >= seatY && y < seatY+25 && x >= left && x < right
			inBack := x >= left && x < left+25 && y >= seatY-130 && y < seatY
			inLeg := y >= seatY+25 && y < seatY+130 && ((x >= left && x < left+15) || (x >= right-15 && x < right))
			if inSeat || inBack || inLeg {
				c = fg
			}
			n := uint8(r.Intn(6))
			c.R, c.G, c.B = c.R^n, c.G^n, c.B^n
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

// ---- items / bids ----

var titleA = []string{"ヴィンテージ", "北欧", "美品", "希少", "レトロ", "アンティーク", "ほぼ新品", "限定", "訳あり", "職人手作り", "昭和", "モダン"}
var titleB = []string{"椅子", "チェア", "スツール", "ソファ", "ベンチ", "座椅子", "ロッキングチェア", "オフィスチェア", "ゲーミングチェア", "ダイニングチェア"}
var titleC = []string{"オーク材", "ウォールナット", "革張り", "ファブリック", "スチール", "籐", "チーク", "メッシュ", "ベルベット", "パイン材"}
var descParts = []string{
	"自宅で大切に使っていました。", "目立つ傷や汚れはありません。", "座り心地は抜群です。", "引っ越しのため出品します。",
	"写真の色味は実物と多少異なります。", "ノークレーム・ノーリターンでお願いします。", "脚に小さなぐらつきがあります。",
	"背もたれの角度は調整できます。", "ペット・喫煙者はいない環境です。", "組み立て済みの状態で発送します。",
}

// Bid は入札（自動入札の上限額）。
type Bid struct {
	User int64
	Max  int
	At   time.Time
}

// priceOf は入札の集合から現在価格と最高入札者を求める。
//
//   - 入札者ごとに最新の上限額だけを使う
//   - 最高入札者 = 上限額が最大の入札者（同額なら先に入札した方）
//   - 現在価格 = 入札者が 1 人なら開始価格、2 人以上なら min(最高額, 2 番目の額 + 入札単位)
func priceOf(start, inc int, bids []Bid) (price int, leader int64) {
	type um struct {
		user  int64
		max   int
		first time.Time
	}
	latest := map[int64]*um{}
	for _, b := range bids {
		if u, ok := latest[b.User]; ok {
			u.max, u.first = b.Max, b.At
		} else {
			latest[b.User] = &um{b.User, b.Max, b.At}
		}
	}
	if len(latest) == 0 {
		return start, 0
	}
	list := make([]*um, 0, len(latest))
	for _, u := range latest {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].max != list[j].max {
			return list[i].max > list[j].max
		}
		return list[i].first.Before(list[j].first)
	})
	if len(list) == 1 {
		return start, list[0].user
	}
	p := list[1].max + inc
	if p > list[0].max {
		p = list[0].max
	}
	return p, list[0].user
}

func genItems() {
	ib := newBatcher("items", "id,seller_id,category_id,title,description,image_id,start_price,increment,status,end_at,original_end_at,winner_id,final_price,created_at")
	bb := newBatcher("bids", "id,item_id,user_id,max_amount,created_at")
	var bidID int64
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 1; i <= NumEnded+NumOpen; i++ {
		id := int64(i)
		open := i > NumEnded
		seller := int64(1 + rng.Intn(NumUsers))
		cat := int64(1 + rng.Intn(len(categories)))
		title := fmt.Sprintf("%s %s %s", titleA[rng.Intn(len(titleA))], titleC[rng.Intn(len(titleC))], titleB[rng.Intn(len(titleB))])
		var desc strings.Builder
		for k := 0; k < 3+rng.Intn(4); k++ {
			desc.WriteString(descParts[rng.Intn(len(descParts))])
		}
		start := (1 + rng.Intn(50)) * 500
		inc := 100
		if start >= 10000 {
			inc = 500
		}

		var created, end time.Time
		if open {
			created = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i-NumEnded) * time.Minute)
			end = farFuture.Add(-time.Duration(rng.Intn(86400*30)) * time.Second)
		} else {
			created = base.Add(time.Duration(i) * 7 * time.Minute)
			end = created.Add(time.Duration(1+rng.Intn(7)) * 24 * time.Hour)
		}

		// 入札を時系列で生成する（各入札はその時点の最低入札額以上）
		n := 0
		if rng.Intn(10) < 8 {
			n = 1 + rng.Intn(16)
		}
		var bids []Bid
		t := created.Add(time.Hour)
		step := end.Sub(created) / time.Duration(n+2)
		if open {
			step = time.Duration(1+rng.Intn(60)) * time.Minute
		}
		for k := 0; k < n; k++ {
			price, leader := priceOf(start, inc, bids)
			user := int64(1 + rng.Intn(NumUsers))
			if user == seller {
				continue
			}
			min := start
			if len(bids) > 0 {
				min = price + inc
			}
			if user == leader {
				continue
			}
			max := min + rng.Intn(10)*inc
			t = t.Add(time.Duration(rng.Int63n(int64(step) + 1)))
			bids = append(bids, Bid{user, max, t})
			bidID++
			bb.add(d(bidID), d(id), d(user), fmt.Sprint(max), ts(t))
		}

		status, winner, final := "open", "NULL", "NULL"
		if !open {
			status = "ended"
			if len(bids) > 0 {
				p, l := priceOf(start, inc, bids)
				winner, final = d(l), fmt.Sprint(p)
			}
		}
		ib.add(d(id), d(seller), d(cat), q(title), q(desc.String()), d(int64(1+rng.Intn(NumImages))),
			fmt.Sprint(start), fmt.Sprint(inc), q(status), ts(end), ts(end), winner, final, ts(created))
	}
	ib.flush()
	bb.flush()
}

// ---- watches / notifications ----

func genWatches() {
	b := newBatcher("watches", "user_id,item_id,created_at")
	seen := map[[2]int64]bool{}
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for len(seen) < NumWatches {
		u := int64(1 + rng.Intn(NumUsers))
		// 7 割は出品中の商品、残りは終了済みの商品
		var it int64
		if rng.Intn(10) < 7 {
			it = int64(NumEnded + 1 + rng.Intn(NumOpen))
		} else {
			it = int64(1 + rng.Intn(NumEnded))
		}
		k := [2]int64{u, it}
		if seen[k] {
			continue
		}
		seen[k] = true
		b.add(d(u), d(it), ts(created.Add(time.Duration(len(seen))*time.Second)))
	}
	b.flush()
}

func genNotifications() {
	b := newBatcher("notifications", "id,user_id,type,item_id,price,created_at")
	types := []string{"outbid", "won", "sold", "ended"}
	created := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= NumNotices; i++ {
		b.add(d(int64(i)), d(int64(1+rng.Intn(NumUsers))), q(types[rng.Intn(len(types))]), d(int64(1+rng.Intn(NumEnded))),
			fmt.Sprint((1+rng.Intn(100))*500), ts(created.Add(time.Duration(i)*time.Minute)))
	}
	b.flush()
}
