package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Product struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Price    int64  `json:"price_minor"` // Integer minor units, e.g. cents.
	Currency string `json:"currency"`
}

type OrderItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

type Order struct {
	ID              string      `json:"id"`
	Items           []OrderItem `json:"items"`
	Amount           int64       `json:"amount_minor"`
	Currency         string      `json:"currency"`
	Status           string      `json:"status"`
	PaymentSessionID string      `json:"-"`
	CheckoutURL      string      `json:"checkout_url,omitempty"`
	CreatedAt        time.Time   `json:"created_at"`
}

type PaymentGateway interface {
	CreateCheckout(context.Context, Order) (sessionID, checkoutURL string, err error)
}

// HTTPGateway speaks a provider-neutral hosted-checkout protocol. Card data is
// entered on the payment provider's page and never handled by this service.
type HTTPGateway struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

func (g *HTTPGateway) CreateCheckout(ctx context.Context, order Order) (string, string, error) {
	payload, err := json.Marshal(struct {
		Reference  string `json:"reference"`
		Amount     int64  `json:"amount_minor"`
		Currency   string `json:"currency"`
		SuccessURL string `json:"success_url,omitempty"`
	}{Reference: order.ID, Amount: order.Amount, Currency: order.Currency})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.BaseURL, "/")+"/checkout-sessions", bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.APIKey)
	resp, err := g.Client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("payment provider returned %s", resp.Status)
	}
	var result struct {
		SessionID   string `json:"session_id"`
		CheckoutURL string `json:"checkout_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", "", err
	}
	if result.SessionID == "" || result.CheckoutURL == "" {
		return "", "", errors.New("payment provider returned an incomplete checkout session")
	}
	return result.SessionID, result.CheckoutURL, nil
}

type Store struct {
	mu         sync.Mutex
	products   map[string]Product
	orders     map[string]Order
	usedEvents map[string]bool
	nextID     uint64
	gateway    PaymentGateway
	webhookKey []byte
}

func newStore(gateway PaymentGateway, webhookKey string) *Store {
	return &Store{
		products: map[string]Product{
			"tee":   {ID: "tee", Name: "Cotton T-shirt", Price: 2500, Currency: "USD"},
			"mug":   {ID: "mug", Name: "Ceramic mug", Price: 1200, Currency: "USD"},
			"tote":  {ID: "tote", Name: "Canvas tote", Price: 1800, Currency: "USD"},
		},
		orders:     make(map[string]Order),
		usedEvents: make(map[string]bool),
		gateway:    gateway,
		webhookKey: []byte(webhookKey),
	}
}

func main() {
	apiURL, apiKey, webhookKey := os.Getenv("PAYMENT_API_URL"), os.Getenv("PAYMENT_API_KEY"), os.Getenv("PAYMENT_WEBHOOK_SECRET")
	if apiURL == "" || apiKey == "" || webhookKey == "" {
		log.Fatal("set PAYMENT_API_URL, PAYMENT_API_KEY, and PAYMENT_WEBHOOK_SECRET")
	}
	store := newStore(&HTTPGateway{BaseURL: apiURL, APIKey: apiKey, Client: &http.Client{Timeout: 10 * time.Second}}, webhookKey)
	mux := http.NewServeMux()
	mux.HandleFunc("/products", store.handleProducts)
	mux.HandleFunc("/orders", store.handleOrders)
	mux.HandleFunc("/orders/", store.handleOrderRoutes)
	mux.HandleFunc("/payments/webhook", store.handleWebhook)
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: withCORS(mux), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("e-commerce API listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}

func (s *Store) handleProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/products" {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	products := make([]Product, 0, len(s.products))
	for _, product := range s.products {
		products = append(products, product)
	}
	writeJSON(w, http.StatusOK, products)
}

func (s *Store) handleOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		Items []OrderItem `json:"items"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if len(input.Items) == 0 || len(input.Items) > 50 {
		writeError(w, http.StatusBadRequest, "order must contain 1 to 50 items")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var amount int64
	for _, item := range input.Items {
		product, ok := s.products[item.ProductID]
		if !ok || item.Quantity < 1 || item.Quantity > 100 {
			writeError(w, http.StatusBadRequest, "unknown product or invalid quantity")
			return
		}
		amount += product.Price * int64(item.Quantity)
		if amount > 100000000 {
			writeError(w, http.StatusBadRequest, "order total is too large")
			return
		}
	}
	s.nextID++
	id := fmt.Sprintf("ord_%d", s.nextID)
	order := Order{ID: id, Items: input.Items, Amount: amount, Currency: "USD", Status: "pending", CreatedAt: time.Now().UTC()}
	s.orders[id] = order
	writeJSON(w, http.StatusCreated, order)
}

func (s *Store) handleOrderRoutes(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "orders" || parts[2] != "checkout" || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.mu.Lock()
	order, ok := s.orders[parts[1]]
	if !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	if order.Status != "pending" {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "order is not payable")
		return
	}
	if order.PaymentSessionID != "" {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"checkout_url": order.CheckoutURL})
		return
	}
	// Keep the lock while creating a session to avoid duplicate provider sessions.
	sessionID, checkoutURL, err := s.gateway.CreateCheckout(r.Context(), order)
	if err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusBadGateway, "could not create payment session")
		return
	}
	order.PaymentSessionID, order.CheckoutURL = sessionID, checkoutURL
	s.orders[order.ID] = order
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"checkout_url": checkoutURL})
}

func (s *Store) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook body")
		return
	}
	provided, err := hex.DecodeString(r.Header.Get("X-Payment-Signature"))
	mac := hmac.New(sha256.New, s.webhookKey)
	_, _ = mac.Write(body)
	if err != nil || !hmac.Equal(provided, mac.Sum(nil)) {
		writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	var event struct {
		ID        string `json:"event_id"`
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(body, &event); err != nil || event.ID == "" || event.SessionID == "" || (event.Status != "paid" && event.Status != "failed" && event.Status != "cancelled") {
		writeError(w, http.StatusBadRequest, "invalid payment event")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usedEvents[event.ID] {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already processed"})
		return
	}
	for id, order := range s.orders {
		if order.PaymentSessionID == event.SessionID {
			if order.Status == "pending" {
				order.Status = event.Status
				s.orders[id] = order
			}
			s.usedEvents[event.ID] = true
			writeJSON(w, http.StatusOK, map[string]string{"status": "processed"})
			return
		}
	}
	writeError(w, http.StatusNotFound, "payment session not found")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Payment-Signature")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
