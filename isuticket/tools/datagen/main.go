// ISUTICKET の初期データ生成器。
// 固定 seed で決定的に SQL を標準出力へ書き出す。
//
//	go run . > initial.sql && mysql isuticket < initial.sql
//
// ベンチマーカーは以下の規則を前提にしている（変更時は bench 側も合わせる）:
//   - 一般ユーザー: login_name = fan%05d (1..NumFans)、password = "isu-" + login_name
//   - 主催者: login_name = organizer、password = organizer-pass
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"
)

const (
	NumFans        = 50000
	NumVenues      = 30
	NumPastEvents  = 400
	NumOnSale      = 70
	NumUpcoming    = 30
	batchSize      = 1000
	baseTimeLayout = "2006-01-02 15:04:05.000000"
)

var (
	rng = rand.New(rand.NewSource(20261019))
	out *bufio.Writer

	farFuture = time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC)
	longAgo   = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
)

type Section struct {
	Name string `json:"name"`
	Rank string `json:"rank"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type Venue struct {
	ID     int64
	Name   string
	Layout []Section
}

func main() {
	out = bufio.NewWriterSize(os.Stdout, 1<<20)
	defer out.Flush()

	fmt.Fprintln(out, "SET NAMES utf8mb4;")
	fmt.Fprintln(out, "SET autocommit=0; SET unique_checks=0; SET foreign_key_checks=0;")

	genUsers()
	venues := genVenues()
	genEvents(venues)

	fmt.Fprintln(out, "COMMIT;")
}

func ts(t time.Time) string { return "'" + t.UTC().Format(baseTimeLayout) + "'" }

func q(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func randHex(n int) string {
	b := make([]byte, n/2)
	rng.Read(b)
	return hex.EncodeToString(b)
}

// batcher は複数行 INSERT をまとめて書き出す。
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

func d(v int64) string { return fmt.Sprint(v) }

// ---- users ----

var familyNames = []string{"椅子", "伊須", "井洲", "石津", "泉", "磯", "井上", "今井", "岩田", "市川", "飯田", "石田", "稲葉", "入江", "犬飼"}
var givenNames = []string{"太郎", "花子", "ひなた", "蓮", "陽葵", "湊", "結衣", "大翔", "さくら", "悠真", "葵", "颯", "凛", "樹", "楓", "奏"}

func passwordHash(salt, password string) string {
	h := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(h[:])
}

func genUsers() {
	b := newBatcher("users", "id,login_name,display_name,password_hash,salt,is_organizer,created_at")
	created := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	salt := randHex(16)
	b.add("1", q("organizer"), q("ISUTICKET 運営"), q(passwordHash(salt, "organizer-pass")), q(salt), "1", ts(created))
	for i := 1; i <= NumFans; i++ {
		login := fmt.Sprintf("fan%05d", i)
		name := familyNames[rng.Intn(len(familyNames))] + givenNames[rng.Intn(len(givenNames))]
		salt := randHex(16)
		b.add(d(int64(i+1)), q(login), q(name), q(passwordHash(salt, "isu-"+login)), q(salt), "0",
			ts(created.Add(time.Duration(i)*time.Minute)))
	}
	b.flush()
}

func fanUserID(i int) int64 { return int64(i + 1) } // fan00001 → id 2

// ---- venues ----

var venueKinds = []struct {
	suffix  string
	weight  int
	minSeat int
	maxSeat int
}{
	{"ホール", 12, 500, 900},
	{"アリーナ", 12, 1000, 1800},
	{"ドーム", 6, 2200, 3200},
}

var cityNames = []string{"椅子市", "伊須町", "井洲浜", "isu 港", "ISU 台", "空気椅子", "背もたれ", "肘掛", "座面", "脚"}

func genVenues() []Venue {
	b := newBatcher("venues", "id,name,layout")
	var venues []Venue
	id := int64(1)
	for _, k := range venueKinds {
		for i := 0; i < k.weight; i++ {
			target := k.minSeat + rng.Intn(k.maxSeat-k.minSeat)
			layout := makeLayout(target)
			name := fmt.Sprintf("%s%s %d", cityNames[rng.Intn(len(cityNames))], k.suffix, i+1)
			lj, _ := json.Marshal(layout)
			b.add(d(id), q(name), q(string(lj)))
			venues = append(venues, Venue{ID: id, Name: name, Layout: layout})
			id++
		}
	}
	b.flush()
	return venues
}

// makeLayout は S/A/B の 3 ランクに座席を割り振る（S:15% A:35% B:50%）。
func makeLayout(target int) []Section {
	var secs []Section
	type part struct {
		rank  string
		ratio float64
		names []string
	}
	parts := []part{
		{"S", 0.15, []string{"アリーナ"}},
		{"A", 0.35, []string{"スタンド1階", "スタンド2階"}},
		{"B", 0.50, []string{"バルコニー", "見切れ席", "立見エリア"}},
	}
	for _, p := range parts {
		seats := int(float64(target) * p.ratio)
		per := seats / len(p.names)
		for _, n := range p.names {
			cols := 20 + rng.Intn(21)
			rows := int(math.Max(1, math.Round(float64(per)/float64(cols))))
			secs = append(secs, Section{Name: n, Rank: p.rank, Rows: rows, Cols: cols})
		}
	}
	return secs
}

// ---- events ----

var artistsA = []string{"椅子", "ISU", "空気", "パイプ", "背もたれ", "回転", "折りたたみ", "ゲーミング", "座布団", "ロッキング"}
var artistsB = []string{"ガールズ", "ボーイズ", "オーケストラ", "楽団", "ブラザーズ", "シスターズ", "48", "Quartet", "Band", "Project"}
var tourNames = []string{"全国ツアー", "単独公演", "アニバーサリーライブ", "ファンミーティング", "フェス", "凱旋公演", "初ワンマン", "カウントダウン"}

type seatRow struct {
	id       int64
	rank     string
	section  string
	row, col int
}

func genEvents(venues []Venue) {
	eb := newBatcher("events", "id,venue_id,title,artist,description,sale_type,sale_start_at,sale_end_at,announce_at,lottery_drawn,gate_open_at,end_at,image,created_at")
	eb.size = 20 // 画像を含むので 1 文を小さく保つ
	rb := newBatcher("event_ranks", "event_id,`rank`,price")
	sb := newBatcher("event_seats", "id,event_id,section,row_no,col_no,`rank`,status,ticket_id,updated_at")
	ob := newBatcher("orders", "id,user_id,event_id,kind,total,payment_id,created_at")
	tb := newBatcher("tickets", "id,order_id,event_id,event_seat_id,user_id,serial,status,used_at,created_at")
	lb := newBatcher("lottery_entries", "id,event_id,user_id,`rank`,quantity,status,created_at")
	slb := newBatcher("resale_listings", "id,ticket_id,event_id,seller_id,price,status,buyer_id,created_at,sold_at")

	var seatID, orderID, ticketID, entryID, listingID int64
	total := NumPastEvents + NumOnSale + NumUpcoming
	pastStart := time.Date(2025, 1, 10, 9, 0, 0, 0, time.UTC)

	for i := 1; i <= total; i++ {
		eventID := int64(i)
		v := venues[rng.Intn(len(venues))]
		artist := artistsA[rng.Intn(len(artistsA))] + artistsB[rng.Intn(len(artistsB))]
		title := fmt.Sprintf("%s %s %d", artist, tourNames[rng.Intn(len(tourNames))], 2025+rng.Intn(2))
		desc := fmt.Sprintf("%s が %s にやってくる！ 椅子の数だけドラマがある。全席指定。", artist, v.Name)

		var kind string // past / onsale / upcoming
		switch {
		case i <= NumPastEvents:
			kind = "past"
		case i <= NumPastEvents+NumOnSale:
			kind = "onsale"
		default:
			kind = "upcoming"
		}

		saleType := "first_come"
		if kind == "past" && rng.Intn(10) < 4 {
			saleType = "lottery"
		}
		if kind == "upcoming" && rng.Intn(2) == 0 {
			saleType = "lottery"
		}

		var saleStart, saleEnd, gateOpen, endAt time.Time
		announce := "NULL"
		drawn := "0"
		switch kind {
		case "past":
			endAt = pastStart.Add(time.Duration(i) * 30 * time.Hour)
			gateOpen = endAt.Add(-4 * time.Hour)
			saleStart = endAt.Add(-60 * 24 * time.Hour)
			saleEnd = endAt.Add(-7 * 24 * time.Hour)
			if saleType == "lottery" {
				announce = ts(saleEnd.Add(24 * time.Hour))
				drawn = "1"
			}
		case "onsale":
			saleStart, saleEnd = longAgo, farFuture
			gateOpen, endAt = farFuture, farFuture.Add(4*time.Hour)
		case "upcoming":
			saleStart = farFuture.Add(-30 * 24 * time.Hour)
			saleEnd = farFuture.Add(-10 * 24 * time.Hour)
			gateOpen, endAt = farFuture, farFuture.Add(4*time.Hour)
			if saleType == "lottery" {
				announce = ts(saleEnd.Add(24 * time.Hour))
			}
		}
		created := saleStart.Add(-14 * 24 * time.Hour)
		if kind != "past" {
			created = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
		}

		prices := map[string]int{
			"S": (12 + rng.Intn(10)) * 1000,
			"A": (8 + rng.Intn(4)) * 1000,
			"B": (5 + rng.Intn(3)) * 1000,
		}
		for _, r := range []string{"S", "A", "B"} {
			rb.add(d(eventID), q(r), fmt.Sprint(prices[r]))
		}

		eb.add(d(eventID), d(v.ID), q(title), q(artist), q(desc), q(saleType),
			ts(saleStart), ts(saleEnd), announce, drawn, ts(gateOpen), ts(endAt),
			"0x"+hex.EncodeToString(genImage(eventID)), ts(created))

		// 座席
		var seats []seatRow
		for _, sec := range v.Layout {
			for r := 1; r <= sec.Rows; r++ {
				for c := 1; c <= sec.Cols; c++ {
					seatID++
					seats = append(seats, seatRow{seatID, sec.Rank, sec.Name, r, c})
				}
			}
		}

		soldRatio := 0.0
		switch kind {
		case "past":
			soldRatio = 0.75 + rng.Float64()*0.25
		case "onsale":
			soldRatio = 0.2 + rng.Float64()*0.4
		}

		// どの座席を売るか決め、注文単位（1〜4 枚）にまとめる
		sold := make(map[int64]int64) // seatID → ticketID
		perm := rng.Perm(len(seats))
		nSold := int(float64(len(seats)) * soldRatio)
		type grp struct {
			user  int64
			seats []seatRow
		}
		var groups []grp
		for k := 0; k < nSold; {
			n := 1 + rng.Intn(4)
			g := grp{user: fanUserID(1 + rng.Intn(NumFans))}
			for j := 0; j < n && k < nSold; j++ {
				g.seats = append(g.seats, seats[perm[k]])
				k++
			}
			groups = append(groups, g)
		}

		orderKind := saleType
		saleT := saleStart.Add(time.Duration(rng.Intn(3600)) * time.Second)
		if saleType == "lottery" && kind == "past" {
			saleT = saleEnd.Add(24 * time.Hour)
		}
		for _, g := range groups {
			orderID++
			sum := 0
			for _, s := range g.seats {
				sum += prices[s.rank]
			}
			payment := q("pay-" + randHex(24))
			if orderKind == "lottery" {
				payment = "NULL"
			}
			ob.add(d(orderID), d(g.user), d(eventID), q(orderKind), fmt.Sprint(sum), payment, ts(saleT))
			for _, s := range g.seats {
				ticketID++
				sold[s.id] = ticketID
				status := "valid"
				used := "NULL"
				if kind == "past" && rng.Intn(10) < 9 {
					used = ts(gateOpen.Add(time.Duration(rng.Intn(3*3600)) * time.Second))
				}
				if kind == "onsale" && rng.Intn(30) == 0 {
					status = "listed"
					listingID++
					slb.add(d(listingID), d(ticketID), d(eventID), d(g.user),
						fmt.Sprint(prices[s.rank]-rng.Intn(3)*500), q("listed"), "NULL",
						ts(saleT.Add(time.Hour)), "NULL")
				}
				tb.add(d(ticketID), d(orderID), d(eventID), d(s.id), d(g.user), q(randHex(32)),
					q(status), used, ts(saleT))
			}
			saleT = saleT.Add(time.Duration(rng.Intn(20)) * time.Second)
		}

		for _, s := range seats {
			if tid, ok := sold[s.id]; ok {
				sb.add(d(s.id), d(eventID), q(s.section), fmt.Sprint(s.row), fmt.Sprint(s.col), q(s.rank), q("sold"), d(tid), ts(saleT))
			} else {
				sb.add(d(s.id), d(eventID), q(s.section), fmt.Sprint(s.row), fmt.Sprint(s.col), q(s.rank), q("available"), "NULL", ts(created))
			}
		}

		// 過去の抽選公演: 当選者（＝購入者）に加えて落選者を作る
		if saleType == "lottery" && kind == "past" {
			for _, g := range groups {
				entryID++
				lb.add(d(entryID), d(eventID), d(g.user), q(g.seats[0].rank), fmt.Sprint(len(g.seats)), q("won"),
					ts(saleStart.Add(time.Duration(rng.Intn(86400*30))*time.Second)))
			}
			lost := len(groups) * (1 + rng.Intn(3))
			for k := 0; k < lost; k++ {
				entryID++
				lb.add(d(entryID), d(eventID), d(fanUserID(1+rng.Intn(NumFans))), q([]string{"S", "A", "B"}[rng.Intn(3)]),
					fmt.Sprint(1+rng.Intn(4)), q("lost"), ts(saleStart.Add(time.Duration(rng.Intn(86400*30))*time.Second)))
			}
		}
	}
	for _, b := range []*batcher{eb, rb, sb, ob, tb, lb, slb} {
		b.flush()
	}
}

// genImage は公演ごとの決定的なポスター画像（JPEG, 640x360）を作る。
func genImage(id int64) []byte {
	r := rand.New(rand.NewSource(id))
	const w, h = 640, 360
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	c1 := color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255}
	c2 := color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255}
	type circle struct {
		x, y, rad int
		c         color.RGBA
	}
	var cs []circle
	for i := 0; i < 12; i++ {
		cs = append(cs, circle{r.Intn(w), r.Intn(h), 10 + r.Intn(80),
			color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255}})
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64(x+y) / float64(w+h)
			px := color.RGBA{
				uint8(float64(c1.R)*(1-t) + float64(c2.R)*t),
				uint8(float64(c1.G)*(1-t) + float64(c2.G)*t),
				uint8(float64(c1.B)*(1-t) + float64(c2.B)*t), 255}
			for _, c := range cs {
				dx, dy := x-c.x, y-c.y
				if dx*dx+dy*dy < c.rad*c.rad {
					px = c.c
				}
			}
			// 軽いノイズ
			n := uint8(r.Intn(12))
			px.R, px.G, px.B = px.R^n, px.G^n, px.B^n
			img.Set(x, y, px)
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}
