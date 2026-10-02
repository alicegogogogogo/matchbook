package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"matchbook/internal/engine"
	"matchbook/internal/server"
)

// errorEnvelope mirrors the single public error shape.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newAPI(t *testing.T) (http.Handler, *engine.Engine) {
	t.Helper()
	matching, err := engine.NewEngine(t.TempDir(), func() int64 { return 1700000000000 })
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { matching.Close() })
	return server.New(matching).Handler(), matching
}

// send drives one request through the router and returns the recorder.
func send(t *testing.T, handler http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decode[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("response body is not valid JSON: %v (%s)", err, response.Body.String())
	}
	return value
}

func expectStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("expected status %d, got %d: %s", status, response.Code, response.Body.String())
	}
}

func expectError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	expectStatus(t, response, status)
	envelope := decode[errorEnvelope](t, response)
	if envelope.Error.Code != code {
		t.Fatalf("expected error code %q, got %q (%s)", code, envelope.Error.Code, response.Body.String())
	}
	if envelope.Error.Message == "" {
		t.Fatalf("error message must not be empty")
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("error responses must be JSON")
	}
}

func createSymbol(t *testing.T, handler http.Handler, body string, key string) {
	t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	response := send(t, handler, http.MethodPost, "/symbols", body, headers)
	expectStatus(t, response, http.StatusCreated)
}

func TestHealthContract(t *testing.T) {
	handler, _ := newAPI(t)
	response := send(t, handler, http.MethodGet, "/health", "", nil)
	expectStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("health must be JSON")
	}
	health := decode[map[string]string](t, response)
	if health["status"] != "ok" {
		t.Fatalf("unexpected health body: %v", health)
	}
}

func TestCreateSymbolIsValidatedAndIdempotent(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":2}`, "sym-1")

	repeat := send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME","price_scale":2}`, map[string]string{"Idempotency-Key": "sym-1"})
	expectStatus(t, repeat, http.StatusCreated)

	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME","price_scale":2}`, map[string]string{"Idempotency-Key": "sym-2"}), http.StatusConflict, "conflict")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME","price_scale":2,"extra":1}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME"}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME","price_scale":"two"}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", ``, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME","price_scale":2} {"symbol":"X","price_scale":2}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME","price_scale":9}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"ACME","price_scale":2}`, map[string]string{"Idempotency-Key": "sym-1" + "x"}), http.StatusConflict, "conflict")

	listed := send(t, handler, http.MethodGet, "/symbols", "", nil)
	expectStatus(t, listed, http.StatusOK)
	payload := decode[struct {
		Symbols []engine.Symbol `json:"symbols"`
	}](t, listed)
	if len(payload.Symbols) != 1 || payload.Symbols[0].Name != "ACME" || payload.Symbols[0].PriceScale != 2 {
		t.Fatalf("unexpected symbol listing: %+v", payload.Symbols)
	}
}

func TestOrderLifecycleOverHTTP(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":2}`, "")

	placed := send(t, handler, http.MethodPost, "/orders", `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":40,"limit_price":1010}`, map[string]string{"Idempotency-Key": "o-1"})
	expectStatus(t, placed, http.StatusCreated)
	resting := decode[engine.OrderView](t, placed)
	if resting.Status != "open" || resting.RemainingQty != 40 || *resting.LimitPrice != "10.10" {
		t.Fatalf("unexpected resting order: %+v", resting)
	}

	crossing := send(t, handler, http.MethodPost, "/orders", `{"id":"bid-1","symbol":"ACME","side":"buy","type":"limit","quantity":40,"limit_price":1010}`, map[string]string{"Idempotency-Key": "o-2"})
	expectStatus(t, crossing, http.StatusCreated)
	filled := decode[engine.OrderView](t, crossing)
	if filled.Status != "filled" || filled.FilledQty != 40 || filled.RemainingQty != 0 {
		t.Fatalf("expected a full fill: %+v", filled)
	}
	if filled.CreatedAt != 1700000000000 {
		t.Fatalf("timestamps must come from the injected clock: %+v", filled)
	}

	bookResponse := send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil)
	expectStatus(t, bookResponse, http.StatusOK)
	book := decode[engine.BookView](t, bookResponse)
	if book.Crossed || book.OpenOrders != 0 || len(book.Bids) != 0 || len(book.Asks) != 0 {
		t.Fatalf("book must be flat after a clean cross: %+v", book)
	}
	if book.LastTrade == nil || *book.LastTrade != "10.10" || book.PriceScale != 2 {
		t.Fatalf("last trade price must be exposed: %+v", book)
	}

	trades := send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil)
	expectStatus(t, trades, http.StatusOK)
	page := decode[engine.TradePage](t, trades)
	if page.Total != 1 || len(page.Trades) != 1 {
		t.Fatalf("expected one trade: %+v", page)
	}
	trade := page.Trades[0]
	if trade.Price != "10.10" || trade.PriceTicks != 1010 || trade.MakerOrder != "ask-1" || trade.TakerOrder != "bid-1" || trade.MakerSide != "sell" {
		t.Fatalf("trade must report the maker price: %+v", trade)
	}

	paged := send(t, handler, http.MethodGet, "/trades?symbol=ACME&limit=1&offset=1", "", nil)
	page = decode[engine.TradePage](t, paged)
	if len(page.Trades) != 0 || page.Total != 1 {
		t.Fatalf("offset past the tape must return an empty page: %+v", page)
	}
}

func TestOrderValidationAndErrors(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"unknown symbol", `{"symbol":"NOPE","side":"buy","type":"limit","quantity":1,"limit_price":10}`, http.StatusNotFound, "not_found"},
		{"bad side", `{"symbol":"ACME","side":"hold","type":"limit","quantity":1,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"bad type", `{"symbol":"ACME","side":"buy","type":"stop","quantity":1,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"missing side", `{"symbol":"ACME","type":"limit","quantity":1,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"missing type", `{"symbol":"ACME","side":"buy","quantity":1,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"missing quantity", `{"symbol":"ACME","side":"buy","type":"limit","limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"missing limit price", `{"symbol":"ACME","side":"buy","type":"limit","quantity":1}`, http.StatusBadRequest, "validation_error"},
		{"market with price", `{"symbol":"ACME","side":"buy","type":"market","quantity":1,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"unknown field", `{"symbol":"ACME","side":"buy","type":"limit","quantity":1,"limit_price":10,"tif":"day"}`, http.StatusBadRequest, "validation_error"},
		{"fractional quantity", `{"symbol":"ACME","side":"buy","type":"limit","quantity":1.5,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"negative quantity", `{"symbol":"ACME","side":"buy","type":"limit","quantity":-1,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"unparsable price", `{"symbol":"ACME","side":"buy","type":"limit","quantity":1,"limit_price":"ten"}`, http.StatusBadRequest, "validation_error"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			expectError(t, send(t, handler, http.MethodPost, "/orders", testCase.body, nil), testCase.status, testCase.code)
		})
	}

	first := send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"buy","type":"limit","quantity":1,"limit_price":10}`, map[string]string{"Idempotency-Key": "same"})
	expectStatus(t, first, http.StatusCreated)
	repeat := send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"buy","type":"limit","quantity":99,"limit_price":99}`, map[string]string{"Idempotency-Key": "same"})
	expectStatus(t, repeat, http.StatusCreated)
	if first.Body.String() != repeat.Body.String() {
		t.Fatalf("repeated key must return the original response:\n%s\n%s", first.Body.String(), repeat.Body.String())
	}
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"OTHER","price_scale":0}`, map[string]string{"Idempotency-Key": "same"}), http.StatusConflict, "conflict")
}

func TestCancelOverHTTP(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")
	send(t, handler, http.MethodPost, "/orders", `{"id":"bid-1","symbol":"ACME","side":"buy","type":"limit","quantity":5,"limit_price":50}`, nil)

	cancelled := send(t, handler, http.MethodDelete, "/orders/bid-1", "", map[string]string{"Idempotency-Key": "c-1"})
	expectStatus(t, cancelled, http.StatusOK)
	view := decode[engine.OrderView](t, cancelled)
	if view.Status != "cancelled" || view.RemainingQty != 5 {
		t.Fatalf("unexpected cancel response: %+v", view)
	}
	retry := send(t, handler, http.MethodDelete, "/orders/bid-1", "", map[string]string{"Idempotency-Key": "c-1"})
	expectStatus(t, retry, http.StatusOK)
	if retry.Body.String() != cancelled.Body.String() {
		t.Fatalf("idempotent cancel must replay the original response")
	}
	expectError(t, send(t, handler, http.MethodDelete, "/orders/bid-1", "", nil), http.StatusConflict, "conflict")
	expectError(t, send(t, handler, http.MethodDelete, "/orders/ghost", "", nil), http.StatusNotFound, "not_found")

	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if book.OpenOrders != 0 || book.Crossed {
		t.Fatalf("cancelled order must leave the book: %+v", book)
	}
}

func TestReplayOverHTTPReportsConsistency(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":2}`, "")

	// Reading an empty book must not change the live sequence, or replay would
	// disagree with the state it just observed.
	empty := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if empty.Sequence != 1 || empty.OpenOrders != 0 {
		t.Fatalf("unexpected empty book: %+v", empty)
	}

	send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"sell","type":"limit","quantity":10,"limit_price":1000}`, nil)
	send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"buy","type":"limit","quantity":4,"limit_price":1000}`, nil)
	send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"buy","type":"market","quantity":3}`, nil)

	type bookCheck struct {
		Consistent    bool `json:"consistent"`
		BookMatched   bool `json:"book_matched"`
		OrdersMatched bool `json:"orders_matched"`
		TradesMatched bool `json:"trades_matched"`
	}
	type replayReport struct {
		Consistent      bool        `json:"consistent"`
		LiveSequence    int         `json:"live_sequence"`
		RebuiltSequence int         `json:"rebuilt_sequence"`
		SymbolCount     int         `json:"symbol_count"`
		Books           []bookCheck `json:"books"`
	}

	response := send(t, handler, http.MethodPost, "/replay", ``, nil)
	expectStatus(t, response, http.StatusOK)
	report := decode[replayReport](t, response)
	if !report.Consistent || report.SymbolCount != 1 {
		t.Fatalf("replay must be consistent: %+v", report)
	}
	if report.LiveSequence != report.RebuiltSequence || report.LiveSequence == 0 {
		t.Fatalf("replay must walk the whole log: %+v", report)
	}
	for _, book := range report.Books {
		if !book.BookMatched || !book.OrdersMatched || !book.TradesMatched || !book.Consistent {
			t.Fatalf("rebuilt book must match the live book: %+v", book)
		}
	}

	filtered := send(t, handler, http.MethodPost, "/replay", `{"symbol":"ACME"}`, nil)
	expectStatus(t, filtered, http.StatusOK)
	if !decode[replayReport](t, filtered).Consistent {
		t.Fatalf("filtered replay must be consistent")
	}
	expectError(t, send(t, handler, http.MethodPost, "/replay", `{"symbol":"NOPE"}`, nil), http.StatusNotFound, "not_found")
	expectError(t, send(t, handler, http.MethodPost, "/replay", `{"symbol":"ACME","extra":true}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/replay", `{"symbol":"ACME"} trailing`, nil), http.StatusBadRequest, "validation_error")
}

func TestQueryAndRoutingErrors(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")

	expectError(t, send(t, handler, http.MethodGet, "/book", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/book?symbol=NOPE", "", nil), http.StatusNotFound, "not_found")
	expectError(t, send(t, handler, http.MethodGet, "/book?symbol=ACME&depth=0", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/book?symbol=ACME&depth=abc", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/book?symbol=ACME&depth=101", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/book?symbol=ACME&size=5", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/book?symbol=ACME&symbol=ACME", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/trades?symbol=ACME&limit=0", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/trades?symbol=ACME&offset=-1", "", nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodGet, "/trades", "", nil), http.StatusBadRequest, "validation_error")

	expectError(t, send(t, handler, http.MethodGet, "/nope", "", nil), http.StatusNotFound, "not_found")
	expectError(t, send(t, handler, http.MethodGet, "/orders", "", nil), http.StatusMethodNotAllowed, "method_not_allowed")
	expectError(t, send(t, handler, http.MethodDelete, "/health", "", nil), http.StatusMethodNotAllowed, "method_not_allowed")
	expectError(t, send(t, handler, http.MethodGet, "/book/", "", nil), http.StatusBadRequest, "validation_error")

	depth := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME&depth=3", "", nil))
	if depth.Depth != 3 || depth.Symbol != "ACME" {
		t.Fatalf("explicit depth must be honoured: %+v", depth)
	}
}

func TestContentTypeAndBodyLimits(t *testing.T) {
	handler, _ := newAPI(t)
	request := httptest.NewRequest(http.MethodPost, "/symbols", strings.NewReader(`{"symbol":"ACME","price_scale":0}`))
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	expectError(t, response, http.StatusBadRequest, "validation_error")

	oversized := strings.Repeat("x", 1<<20+10)
	expectError(t, send(t, handler, http.MethodPost, "/symbols", `{"symbol":"`+oversized+`","price_scale":0}`, nil), http.StatusBadRequest, "validation_error")
}

func TestBookDepthLimitsLevelsButNotCounts(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")
	for i := 1; i <= 6; i++ {
		body := `{"symbol":"ACME","side":"buy","type":"limit","quantity":` + strconv.Itoa(i) + `,"limit_price":` + strconv.Itoa(100-i) + `}`
		expectStatus(t, send(t, handler, http.MethodPost, "/orders", body, nil), http.StatusCreated)
	}
	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME&depth=3", "", nil))
	if len(book.Bids) != 3 || book.BidCount != 6 || book.Depth != 3 {
		t.Fatalf("depth must limit levels but not the reported level count: %+v", book)
	}
	if book.Bids[0].Price != 99 || book.Bids[0].OrderCount != 1 {
		t.Fatalf("levels must be best price first: %+v", book.Bids)
	}
}
