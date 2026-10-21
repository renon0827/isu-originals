package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// PaymentServer は外部決済サービスのモック。
//
//   - 応答まで 30〜200ms かかる
//   - 3% の確率で課金せずに 503 を返す
//   - 2% の確率で課金したうえで 500 を返す（応答が失われたケース）
//
// 同じ Idempotency-Key での再送には、課金済みなら同じ payment_id を返す。
// 再送時にキーを変えると二重課金になる。
type PaymentServer struct {
	mu       sync.Mutex
	byKey    map[string]*Payment
	byID     map[string]*Payment
	seq      atomic.Int64
	requests atomic.Int64
}

type Payment struct {
	ID     string
	Key    string
	Amount int
	At     time.Time
}

func NewPaymentServer() *PaymentServer {
	return &PaymentServer{byKey: map[string]*Payment{}, byID: map[string]*Payment{}}
}

func (s *PaymentServer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey = map[string]*Payment{}
	s.byID = map[string]*Payment{}
}

func (s *PaymentServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/payments" {
		http.NotFound(w, r)
		return
	}
	s.requests.Add(1)
	key := r.Header.Get("Idempotency-Key")
	var req struct {
		Amount int `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || key == "" || req.Amount <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"amount and Idempotency-Key are required"}`))
		return
	}

	time.Sleep(time.Duration(30+rand.Intn(170)) * time.Millisecond)

	s.mu.Lock()
	p, exists := s.byKey[key]
	s.mu.Unlock()
	if exists {
		if p.Amount != req.Amount {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"error":"amount mismatch for the same Idempotency-Key"}`))
			return
		}
		writePayment(w, p)
		return
	}

	x := rand.Float64()
	if x < 0.03 {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"temporarily unavailable"}`))
		return
	}
	p = &Payment{ID: fmt.Sprintf("pay-%08d", s.seq.Add(1)), Key: key, Amount: req.Amount, At: time.Now()}
	s.mu.Lock()
	s.byKey[key] = p
	s.byID[p.ID] = p
	s.mu.Unlock()
	if x < 0.05 {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal error"}`))
		return
	}
	writePayment(w, p)
}

func writePayment(w http.ResponseWriter, p *Payment) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"payment_id": p.ID})
}

func (s *PaymentServer) Get(id string) (*Payment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	return p, ok
}

func (s *PaymentServer) All() []*Payment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Payment, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	return out
}
