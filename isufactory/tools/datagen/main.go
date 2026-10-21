// ISUFACTORY の初期データ生成器。
// 固定 seed で決定的に SQL を標準出力へ書き出す。
//
//	go run . > initial.sql && mysql isufactory < initial.sql
//
// ベンチマーカーは以下の規則を前提にしている（変更時は bench 側も合わせる）:
//   - 現場担当: login_name = operatorNN (01..50)、password = "isu-" + login_name
//   - 計画担当: login_name = plannerNN (01..10)、password = "isu-" + login_name
//   - 設備: 10 ライン × 4 種類。設備 ID = (ライン ID - 1) * 4 + 種類番号 (1..4)
//   - 設備トークン: sha256("isufactory-machine-<ID>") の先頭 32 文字（16 進）
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"
)

const (
	NumLines        = 10
	NumOperators    = 50
	NumPlanners     = 10
	NumProducts     = 60
	NumMaterials    = 30
	NumPastOrders   = 20000
	ReadingsPerKind = 2000 // 設備 × 指標ごとの履歴件数
	NumPastAlerts   = 1500
	batchSize       = 1000
	timeLayout      = "2006-01-02 15:04:05.000000"
)

var (
	rng = rand.New(rand.NewSource(20261021))
	out *bufio.Writer

	MachineTypes = []string{"press", "weld", "paint", "assembly"}
	Families     = []string{"A", "B", "C", "D", "E", "F"}
	Metrics      = []string{"temperature", "vibration", "current"}
	MetricBase   = map[string]float64{"temperature": 60, "vibration": 3, "current": 12}

	// 履歴データの期間（すべて過去）
	historyStart = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	historyEnd   = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
)

func main() {
	out = bufio.NewWriterSize(os.Stdout, 1<<20)
	defer out.Flush()

	fmt.Fprintln(out, "SET NAMES utf8mb4;")
	fmt.Fprintln(out, "SET autocommit=0; SET unique_checks=0; SET foreign_key_checks=0;")

	genUsers()
	genLinesAndMachines()
	products := genProducts()
	genMaterials(products)
	genHistory(products)
	genReadings()

	fmt.Fprintln(out, "COMMIT;")
}

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

func passwordHash(salt, password string) string {
	h := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(h[:])
}

var familyNames = []string{"椅子", "伊須", "井洲", "石津", "泉", "磯", "今井", "岩田", "市川", "飯田"}
var givenNames = []string{"太郎", "花子", "工", "鉄平", "溶子", "組立", "塗", "圧", "仁", "精"}

func genUsers() {
	b := newBatcher("users", "id,login_name,display_name,password_hash,salt,role,created_at")
	created := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	id := int64(0)
	add := func(login, role string) {
		id++
		salt := randHex(16)
		name := familyNames[rng.Intn(len(familyNames))] + givenNames[rng.Intn(len(givenNames))]
		b.add(d(id), q(login), q(name), q(passwordHash(salt, "isu-"+login)), q(salt), q(role), ts(created))
	}
	for i := 1; i <= NumOperators; i++ {
		add(fmt.Sprintf("operator%02d", i), "operator")
	}
	for i := 1; i <= NumPlanners; i++ {
		add(fmt.Sprintf("planner%02d", i), "planner")
	}
	b.flush()
}

// ---- lines / machines ----

func MachineToken(id int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("isufactory-machine-%d", id)))
	return hex.EncodeToString(h[:])[:32]
}

var typeLabel = map[string]string{"press": "プレス機", "weld": "溶接機", "paint": "塗装機", "assembly": "組立機"}

func genLinesAndMachines() {
	lb := newBatcher("lines", "id,name")
	mb := newBatcher("machines", "id,line_id,name,machine_type,token")
	for l := 1; l <= NumLines; l++ {
		lb.add(d(int64(l)), q(fmt.Sprintf("第%d ライン", l)))
		for ti, mt := range MachineTypes {
			id := int64((l-1)*len(MachineTypes) + ti + 1)
			mb.add(d(id), d(int64(l)), q(fmt.Sprintf("L%02d-%s", l, typeLabel[mt])), q(mt), q(MachineToken(id)))
		}
	}
	lb.flush()
	mb.flush()
}

// ---- products / materials ----

type Product struct {
	ID          int64
	Family      string
	MachineType string
	UnitSeconds int
}

var productNouns = []string{"座面", "背もたれ", "脚", "肘掛", "キャスター", "ガスシリンダー", "フレーム", "ネジ", "ブラケット", "ヘッドレスト"}

func genProducts() []Product {
	b := newBatcher("products", "id,name,family,machine_type,unit_seconds,drawing")
	b.size = 20
	var ps []Product
	for i := 1; i <= NumProducts; i++ {
		p := Product{
			ID:          int64(i),
			Family:      Families[rng.Intn(len(Families))],
			MachineType: MachineTypes[(i-1)%len(MachineTypes)],
			UnitSeconds: 10 + rng.Intn(31),
		}
		name := fmt.Sprintf("%s %s-%03d", productNouns[rng.Intn(len(productNouns))], p.Family, i)
		b.add(d(p.ID), q(name), q(p.Family), q(p.MachineType), fmt.Sprint(p.UnitSeconds),
			"0x"+hex.EncodeToString(genDrawing(p.ID)))
		ps = append(ps, p)
	}
	b.flush()
	return ps
}

var materialNames = []string{"鋼板", "パイプ", "ボルト", "ナット", "塗料", "ウレタン", "合板", "樹脂", "布地", "ワッシャー"}

func genMaterials(products []Product) {
	mb := newBatcher("materials", "id,name,stock")
	for i := 1; i <= NumMaterials; i++ {
		mb.add(d(int64(i)), q(fmt.Sprintf("%s #%02d", materialNames[(i-1)%len(materialNames)], i)), fmt.Sprint(4000+rng.Intn(6000)))
	}
	mb.flush()
	bb := newBatcher("bom", "product_id,material_id,quantity")
	for _, p := range products {
		perm := rng.Perm(NumMaterials)
		n := 2 + rng.Intn(3)
		for k := 0; k < n; k++ {
			bb.add(d(p.ID), d(int64(perm[k]+1)), fmt.Sprint(1+rng.Intn(3)))
		}
	}
	bb.flush()
}

// ---- 過去の受注と作業割当 ----

func genHistory(products []Product) {
	ob := newBatcher("orders", "id,product_id,quantity,due_at,status,created_at")
	ab := newBatcher("assignments", "id,order_id,machine_id,family,start_at,end_at,created_at")
	rb := newBatcher("material_receipts", "id,material_id,quantity,created_at")

	byType := map[string][]Product{}
	for _, p := range products {
		byType[p.MachineType] = append(byType[p.MachineType], p)
	}
	// 設備ごとに履歴期間を順に埋める
	type mstate struct {
		id     int64
		typ    string
		cursor time.Time
		family string
	}
	var ms []*mstate
	for l := 1; l <= NumLines; l++ {
		for ti, mt := range MachineTypes {
			ms = append(ms, &mstate{id: int64((l-1)*len(MachineTypes) + ti + 1), typ: mt, cursor: historyStart})
		}
	}
	const setup = 600 * time.Second
	for i := 1; i <= NumPastOrders; i++ {
		m := ms[rng.Intn(len(ms))]
		cands := byType[m.typ]
		p := cands[rng.Intn(len(cands))]
		qty := 20 + rng.Intn(81)
		dur := time.Duration(qty*p.UnitSeconds) * time.Second
		if m.family != "" && m.family != p.Family {
			dur += setup
		}
		start := m.cursor.Add(time.Duration(rng.Intn(600)) * time.Second)
		end := start.Add(dur)
		created := start.Add(-time.Duration(1+rng.Intn(24)) * time.Hour)
		due := end.Add(time.Duration(rng.Intn(240)-60) * time.Minute)
		ob.add(d(int64(i)), d(p.ID), fmt.Sprint(qty), ts(due), q("planned"), ts(created))
		ab.add(d(int64(i)), d(int64(i)), d(m.id), q(p.Family), ts(start), ts(end), ts(created))
		m.cursor = end
		m.family = p.Family
	}
	for i := 1; i <= 2000; i++ {
		rb.add(d(int64(i)), d(int64(1+rng.Intn(NumMaterials))), fmt.Sprint(500+rng.Intn(2000)),
			ts(historyStart.Add(time.Duration(rng.Int63n(int64(historyEnd.Sub(historyStart)))))))
	}
	ob.flush()
	ab.flush()
	rb.flush()
}

// ---- センサー履歴とアラート ----

func genReadings() {
	rb := newBatcher("readings", "id,machine_id,metric,value,measured_at")
	ab := newBatcher("alerts", "id,machine_id,metric,value,baseline,measured_at,created_at")
	numMachines := NumLines * len(MachineTypes)
	step := historyEnd.Sub(historyStart) / ReadingsPerKind
	var id, alertID int64
	// 時刻順に全設備・全指標を交互に並べる（実データの到着順に近づける）
	for k := 0; k < ReadingsPerKind; k++ {
		t := historyStart.Add(time.Duration(k) * step)
		for m := 1; m <= numMachines; m++ {
			for _, metric := range Metrics {
				id++
				base := MetricBase[metric] * (0.9 + 0.02*float64(m%10))
				v := base * (1 + 0.05*math.Sin(float64(k)/50+float64(m)) + (rng.Float64()-0.5)*0.04)
				if alertID < NumPastAlerts && rng.Intn(400) == 0 {
					v = base * (1.8 + rng.Float64()*0.4)
					alertID++
					ab.add(d(alertID), d(int64(m)), q(metric), fmt.Sprintf("%.4f", v), fmt.Sprintf("%.4f", base), ts(t), ts(t))
				}
				rb.add(d(id), d(int64(m)), q(metric), fmt.Sprintf("%.4f", v), ts(t.Add(time.Duration(m)*time.Second)))
			}
		}
	}
	rb.flush()
	ab.flush()
}

// genDrawing は製品ごとの決定的な図面（PNG, 640x400 の線画）を作る。
func genDrawing(id int64) []byte {
	r := rand.New(rand.NewSource(id))
	const w, h = 640, 400
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{0xf4, 0xf6, 0xfa, 255}
	ink := color.RGBA{0x1d, 0x3a, 0x8a, 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := bg
			// 方眼
			if x%20 == 0 || y%20 == 0 {
				c = color.RGBA{0xdd, 0xe3, 0xee, 255}
			}
			img.Set(x, y, c)
		}
	}
	line := func(x0, y0, x1, y1 int) {
		n := int(math.Max(math.Abs(float64(x1-x0)), math.Abs(float64(y1-y0))))
		for i := 0; i <= n; i++ {
			x := x0 + (x1-x0)*i/max(n, 1)
			y := y0 + (y1-y0)*i/max(n, 1)
			img.Set(x, y, ink)
			img.Set(x+1, y, ink)
		}
	}
	for i := 0; i < 8; i++ {
		x0, y0 := 40+r.Intn(w-80), 40+r.Intn(h-80)
		bw, bh := 30+r.Intn(200), 30+r.Intn(150)
		line(x0, y0, x0+bw, y0)
		line(x0+bw, y0, x0+bw, y0+bh)
		line(x0+bw, y0+bh, x0, y0+bh)
		line(x0, y0+bh, x0, y0)
	}
	for i := 0; i < 5; i++ {
		cx, cy, rad := 60+r.Intn(w-120), 60+r.Intn(h-120), 10+r.Intn(50)
		for a := 0; a < 720; a++ {
			th := float64(a) * math.Pi / 360
			img.Set(cx+int(float64(rad)*math.Cos(th)), cy+int(float64(rad)*math.Sin(th)), ink)
		}
	}
	// 寸法線の細かい目盛り
	for i := 0; i < 3000; i++ {
		x, y := r.Intn(w), r.Intn(h)
		img.Set(x, y, color.RGBA{0x8a, 0x95, 0xab, 255})
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}
