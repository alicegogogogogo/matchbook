package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"matchbook/internal/engine"
)

func placeOrder(t *testing.T, handler http.Handler, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	response := send(t, handler, http.MethodPost, "/orders", body, headers)
	expectStatus(t, response, http.StatusCreated)
	return response
}

func TestIOCOrderLifecycleOverHTTP(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":2}`, "")

	placeOrder(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":6,"limit_price":1000}`, "")
	placeOrder(t, handler, `{"id":"ask-2","symbol":"ACME","side":"sell","type":"limit","quantity":4,"limit_price":1030}`, "")

	partial := placeOrder(t, handler, `{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":12,"limit_price":1030}`, "ioc-key")
	view := decode[engine.OrderView](t, partial)
	if view.Type != "ioc" || view.Status != "cancelled" || view.FilledQty != 10 || view.RemainingQty != 2 {
		t.Fatalf("partial ioc must be cancelled with honest quantities: %+v", view)
	}
	if view.LimitPrice == nil || *view.LimitPrice != "10.30" || view.LimitTicks == nil || *view.LimitTicks != 1030 {
		t.Fatalf("ioc must project its limit price with scale: %+v", view)
	}
	if len(view.TradeIDs) != 2 {
		t.Fatalf("ioc must trade both reachable levels: %+v", view.TradeIDs)
	}

	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if book.OpenOrders != 0 || book.AskCount != 0 || book.BidCount != 0 || book.Crossed {
		t.Fatalf("a cancelled ioc remainder must never rest: %+v", book)
	}

	trades := decode[engine.TradePage](t, send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil))
	if trades.Total != 2 ||
		trades.Trades[0].PriceTicks != 1000 || trades.Trades[0].Quantity != 6 || trades.Trades[0].TakerOrder != "ioc-1" ||
		trades.Trades[1].PriceTicks != 1030 || trades.Trades[1].Quantity != 4 || trades.Trades[1].TakerOrder != "ioc-1" {
		t.Fatalf("ioc trades must walk the book at maker prices: %+v", trades.Trades)
	}
}

func TestFOKOrderLifecycleOverHTTP(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")

	placeOrder(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":6,"limit_price":100}`, "")
	placeOrder(t, handler, `{"id":"ask-2","symbol":"ACME","side":"sell","type":"limit","quantity":4,"limit_price":103}`, "")

	rejected := placeOrder(t, handler, `{"id":"fok-no","symbol":"ACME","side":"buy","type":"fok","quantity":11,"limit_price":103}`, "")
	view := decode[engine.OrderView](t, rejected)
	if view.Type != "fok" || view.Status != "rejected" || view.FilledQty != 0 || view.RemainingQty != 11 {
		t.Fatalf("a short fok must be rejected with a zero fill: %+v", view)
	}
	if view.LimitPrice == nil || *view.LimitPrice != "103" {
		t.Fatalf("fok must project its limit price: %+v", view)
	}
	if len(view.TradeIDs) != 0 {
		t.Fatalf("a rejected fok must not trade at all: %+v", view.TradeIDs)
	}
	trades := decode[engine.TradePage](t, send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil))
	if trades.Total != 0 {
		t.Fatalf("fok rejection must be atomic: %+v", trades.Trades)
	}
	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if book.OpenOrders != 2 || book.Asks[0].Quantity != 6 || book.Asks[1].Quantity != 4 {
		t.Fatalf("a rejected fok must leave every maker untouched: %+v", book)
	}

	filled := placeOrder(t, handler, `{"id":"fok-yes","symbol":"ACME","side":"buy","type":"fok","quantity":10,"limit_price":103}`, "")
	ok := decode[engine.OrderView](t, filled)
	if ok.Status != "filled" || ok.FilledQty != 10 || ok.RemainingQty != 0 || len(ok.TradeIDs) != 2 {
		t.Fatalf("a fully supported fok must fill across levels: %+v", ok)
	}
}

func TestIOCOrderWithNoMatchIsCancelledOverHTTP(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")
	placeOrder(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":5,"limit_price":105}`, "")

	response := placeOrder(t, handler, `{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":5,"limit_price":104}`, "")
	view := decode[engine.OrderView](t, response)
	if view.Status != "cancelled" || view.FilledQty != 0 || view.RemainingQty != 5 {
		t.Fatalf("a non-crossing ioc must come back cancelled and unfilled: %+v", view)
	}
}

func TestIOCAndFOKHTTPValidation(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")

	cases := []struct {
		name string
		body string
	}{
		{"unknown type", `{"symbol":"ACME","side":"buy","type":"stop","quantity":1,"limit_price":10}`},
		{"ioc missing price", `{"symbol":"ACME","side":"buy","type":"ioc","quantity":1}`},
		{"fok missing price", `{"symbol":"ACME","side":"buy","type":"fok","quantity":1}`},
		{"ioc zero price", `{"symbol":"ACME","side":"buy","type":"ioc","quantity":1,"limit_price":0}`},
		{"fok fractional price", `{"symbol":"ACME","side":"buy","type":"fok","quantity":1,"limit_price":1.5}`},
		{"ioc unknown symbol", `{"symbol":"NOPE","side":"buy","type":"ioc","quantity":1,"limit_price":10}`},
		{"fok unknown symbol", `{"symbol":"NOPE","side":"buy","type":"fok","quantity":1,"limit_price":10}`},
		{"ioc unknown field", `{"symbol":"ACME","side":"buy","type":"ioc","quantity":1,"limit_price":10,"tif":"ioc"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code := "validation_error"
			status := http.StatusBadRequest
			if testCase.name == "ioc unknown symbol" || testCase.name == "fok unknown symbol" {
				code, status = "not_found", http.StatusNotFound
			}
			expectError(t, send(t, handler, http.MethodPost, "/orders", testCase.body, nil), status, code)
		})
	}

	placeOrder(t, handler, `{"id":"dup","symbol":"ACME","side":"buy","type":"fok","quantity":1,"limit_price":10}`, "")
	expectError(t, send(t, handler, http.MethodPost, "/orders",
		`{"id":"dup","symbol":"ACME","side":"buy","type":"fok","quantity":1,"limit_price":10}`, nil),
		http.StatusConflict, "conflict")
}

func TestIOCAndFOKAreIdempotentOverHTTP(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")
	placeOrder(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":6,"limit_price":100}`, "")
	placeOrder(t, handler, `{"id":"ask-2","symbol":"ACME","side":"sell","type":"limit","quantity":4,"limit_price":103}`, "")

	// The first ioc consumes all ten units and is cancelled with a remainder.
	first := placeOrder(t, handler, `{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":12,"limit_price":103}`, "ioc-repeat")
	repeat := placeOrder(t, handler, `{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":12,"limit_price":103}`, "ioc-repeat")
	if first.Body.String() != repeat.Body.String() {
		t.Fatalf("repeated ioc key must return the first result:\n%s\n%s", first.Body.String(), repeat.Body.String())
	}

	// The first fok is rejected; replaying the key must not turn it into a fill.
	firstFOK := placeOrder(t, handler, `{"id":"fok-1","symbol":"ACME","side":"buy","type":"fok","quantity":11,"limit_price":103}`, "fok-repeat")
	repeatFOK := placeOrder(t, handler, `{"id":"fok-1","symbol":"ACME","side":"buy","type":"fok","quantity":11,"limit_price":103}`, "fok-repeat")
	if firstFOK.Body.String() != repeatFOK.Body.String() {
		t.Fatalf("repeated fok key must return the first rejection:\n%s\n%s", firstFOK.Body.String(), repeatFOK.Body.String())
	}
	view := decode[engine.OrderView](t, repeatFOK)
	if view.Status != "rejected" || view.FilledQty != 0 || view.RemainingQty != 11 {
		t.Fatalf("cached fok result must stay a zero-fill rejection: %+v", view)
	}

	// Exactly the two ioc trades exist; the fok produced none and neither retry
	// changed the book.
	trades := decode[engine.TradePage](t, send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil))
	if trades.Total != 2 {
		t.Fatalf("replayed submissions must not trade again: %+v", trades.Trades)
	}
	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if book.OpenOrders != 0 {
		t.Fatalf("replayed submissions must not change the book: %+v", book)
	}
}

func TestReplayConsistencyAfterIOCAndFOKWorkload(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":2}`, "")

	bodies := []string{
		`{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":6,"limit_price":1000}`,
		`{"id":"ask-2","symbol":"ACME","side":"sell","type":"limit","quantity":4,"limit_price":1030}`,
		`{"id":"bid-1","symbol":"ACME","side":"buy","type":"limit","quantity":3,"limit_price":900}`,
		// ioc: takes both asks, cancelled with 5 left.
		`{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":15,"limit_price":1030}`,
		// fok: nothing on the ask side, rejected without trades.
		`{"id":"fok-1","symbol":"ACME","side":"buy","type":"fok","quantity":2,"limit_price":1030}`,
		// Sell ioc: takes bid-1's 3 units, cancelled with 2 left.
		`{"id":"ioc-s","symbol":"ACME","side":"sell","type":"ioc","quantity":5,"limit_price":900}`,
		// Seed asks again and let a fok sweep them exactly.
		`{"id":"ask-3","symbol":"ACME","side":"sell","type":"limit","quantity":2,"limit_price":1010}`,
		`{"id":"ask-4","symbol":"ACME","side":"sell","type":"limit","quantity":3,"limit_price":1010}`,
		`{"id":"fok-2","symbol":"ACME","side":"buy","type":"fok","quantity":5,"limit_price":1010}`,
	}
	for _, body := range bodies {
		expectStatus(t, send(t, handler, http.MethodPost, "/orders", body, nil), http.StatusCreated)
	}

	type replayReport struct {
		Consistent bool `json:"consistent"`
		Books      []struct {
			Consistent            bool `json:"consistent"`
			BookMatched           bool `json:"book_matched"`
			OrdersMatched         bool `json:"orders_matched"`
			TradesMatched         bool `json:"trades_matched"`
			TradeCount            int  `json:"trade_count"`
			RebuiltTradeCount     int  `json:"rebuilt_trade_count"`
			OpenOrderCount        int  `json:"open_order_count"`
			RebuiltOpenOrderCount int  `json:"rebuilt_open_order_count"`
		} `json:"books"`
	}

	report := decode[replayReport](t, send(t, handler, http.MethodPost, "/replay", `{"symbol":"ACME"}`, nil))
	if !report.Consistent || len(report.Books) != 1 {
		t.Fatalf("replay must stay consistent with ioc/fok orders: %+v", report)
	}
	book := report.Books[0]
	if !book.BookMatched || !book.OrdersMatched || !book.TradesMatched || !book.Consistent {
		t.Fatalf("every rebuilt facet must match: %+v", book)
	}
	// ioc-1 (2 fills) + ioc-s (1 fill) + fok-2 (2 fills) = 5 trades; fok-1 none.
	if book.TradeCount != 5 || book.RebuiltTradeCount != 5 {
		t.Fatalf("rebuilt trade count must include ioc fills and exclude fok rejects: %+v", book)
	}
	if book.OpenOrderCount != 0 || book.RebuiltOpenOrderCount != 0 {
		t.Fatalf("every order in this workload terminates: %+v", book)
	}

	// Trade tape cross-check.
	trades := decode[engine.TradePage](t, send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil))
	if trades.Total != 5 {
		t.Fatalf("expected 5 trades on the tape, got %d", trades.Total)
	}
	want := []struct {
		price int64
		qty   int64
		taker string
	}{
		{1000, 6, "ioc-1"},
		{1030, 4, "ioc-1"},
		{900, 3, "ioc-s"},
		{1010, 2, "fok-2"},
		{1010, 3, "fok-2"},
	}
	for i, fill := range want {
		got := trades.Trades[i]
		if got.PriceTicks != fill.price || got.Quantity != fill.qty || got.TakerOrder != fill.taker {
			t.Fatalf("trade %d = price %d qty %d taker %s, want %+v", i, got.PriceTicks, got.Quantity, got.TakerOrder, fill)
		}
	}
}
