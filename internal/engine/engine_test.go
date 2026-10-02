package engine_test

import (
	"errors"
	"testing"

	"matchbook/internal/engine"
)

// fixedClock is the injected deterministic clock used by every test. The engine
// never calls time.Now on its own, so timestamps are reproducible.
func fixedClock() func() int64 {
	value := int64(1700000000000)
	return func() int64 { return value }
}

func newEngine(t *testing.T) *engine.Engine {
	t.Helper()
	matching, err := engine.NewEngine(t.TempDir(), fixedClock())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { matching.Close() })
	return matching
}

func mustSymbol(t *testing.T, matching *engine.Engine, name string, scale int64) {
	t.Helper()
	if _, err := matching.AddSymbol(name, scale); err != nil {
		t.Fatalf("AddSymbol(%s): %v", name, err)
	}
}

func mustOrder(t *testing.T, matching *engine.Engine, id, symbol, side, orderType string, quantity, price int64) engine.OrderView {
	t.Helper()
	view, err := matching.PlaceOrder(id, symbol, side, orderType, quantity, price)
	if err != nil {
		t.Fatalf("PlaceOrder(%s): %v", id, err)
	}
	return view
}

func bookOf(t *testing.T, matching *engine.Engine, symbol string) engine.BookView {
	t.Helper()
	book, err := matching.Book(symbol, 10)
	if err != nil {
		t.Fatalf("Book(%s): %v", symbol, err)
	}
	return book
}

func TestPriceTimePriorityFillsOldestOrderFirst(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 2)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 40, 1010)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 40, 1010)
	mustOrder(t, matching, "ask-3", "ACME", engine.SideSell, engine.TypeLimit, 40, 1005)

	incoming := mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 50, 1010)
	if incoming.Status != engine.StatusFilled || incoming.FilledQty != 50 || incoming.RemainingQty != 0 {
		t.Fatalf("unexpected incoming order state: %+v", incoming)
	}
	if len(incoming.TradeIDs) != 2 {
		t.Fatalf("expected 2 trades, got %v", incoming.TradeIDs)
	}

	page, err := matching.Trades("ACME", 0, 100)
	if err != nil {
		t.Fatalf("Trades: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("expected 2 trades, got %d", page.Total)
	}
	first, second := page.Trades[0], page.Trades[1]
	if first.PriceTicks != 1005 || first.Quantity != 40 || first.MakerOrder != "ask-3" {
		t.Fatalf("best price must trade first at the maker price: %+v", first)
	}
	if second.PriceTicks != 1010 || second.Quantity != 10 || second.MakerOrder != "ask-1" {
		t.Fatalf("price-time priority must reach the oldest order next: %+v", second)
	}
	if second.Price != "10.10" || first.Price != "10.05" {
		t.Fatalf("prices must be scaled with the symbol price scale: %q %q", first.Price, second.Price)
	}

	book := bookOf(t, matching, "ACME")
	if book.BestAsk == nil || *book.BestAsk != "10.10" || book.AskCount != 1 {
		t.Fatalf("ask-2 and the remainder of ask-1 must share one level at 10.10: %+v", book)
	}
	if book.Asks[0].Quantity != 70 || book.Asks[0].OrderCount != 2 {
		t.Fatalf("the 10.10 level keeps both resting orders: %+v", book.Asks)
	}
	if book.BestBid != nil || book.BidCount != 0 {
		t.Fatalf("a fully filled taker must not rest: %+v", book)
	}
	if book.Crossed {
		t.Fatalf("book must not be crossed: %+v", book)
	}
}

func TestPartialFillLeavesRestingRemainder(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 10, 100)
	resting := mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 25, 100)

	if resting.Status != engine.StatusOpen {
		t.Fatalf("partially filled limit order must stay open: %+v", resting)
	}
	if resting.FilledQty != 10 || resting.RemainingQty != 15 {
		t.Fatalf("quantity must be conserved: %+v", resting)
	}
	book := bookOf(t, matching, "ACME")
	if book.BestBid == nil || *book.BestBid != "100" || book.Bids[0].Quantity != 15 || book.Bids[0].OrderCount != 1 {
		t.Fatalf("remainder must rest at the limit price: %+v", book)
	}
}

func TestLimitPriceEqualToOneTickAboveBestTrades(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	// A buyer at 101 meets a seller at 101: equality is a cross, not a spread.
	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 5, 101)
	view := mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 5, 101)
	if view.Status != engine.StatusFilled {
		t.Fatalf("crossing prices must trade: %+v", view)
	}
	book := bookOf(t, matching, "ACME")
	if book.Crossed || book.BidCount != 0 || book.AskCount != 0 {
		t.Fatalf("book must be empty after a clean cross: %+v", book)
	}
}

func TestNonCrossingLimitOrderRestsWithoutTrading(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 5, 105)
	view := mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 5, 104)
	if view.Status != engine.StatusOpen || len(view.TradeIDs) != 0 {
		t.Fatalf("a non-crossing order must rest untouched: %+v", view)
	}
	book := bookOf(t, matching, "ACME")
	if *book.BestBid != "104" || *book.BestAsk != "105" || book.Crossed {
		t.Fatalf("unexpected two-sided book: %+v", book)
	}
}

func TestMarketOrderSweepsAndRejectsUnfilledRemainder(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 4, 103)

	swept := mustOrder(t, matching, "mkt-1", "ACME", engine.SideBuy, engine.TypeMarket, 10, 0)
	if swept.Status != engine.StatusFilled || swept.FilledQty != 10 {
		t.Fatalf("market order must take every level: %+v", swept)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 2 || page.Trades[0].PriceTicks != 100 || page.Trades[1].PriceTicks != 103 {
		t.Fatalf("market order must walk the book in price order: %+v", page)
	}

	short := mustOrder(t, matching, "mkt-2", "ACME", engine.SideBuy, engine.TypeMarket, 5, 0)
	if short.Status != engine.StatusRejected || short.RemainingQty != 5 || short.FilledQty != 0 {
		t.Fatalf("an unfillable market order must be rejected: %+v", short)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 0 || book.Crossed {
		t.Fatalf("a rejected market order must not rest: %+v", book)
	}
	if _, err := matching.CancelOrder("mkt-2"); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("a rejected order cannot be cancelled, got %v", err)
	}
}

func TestCancelRemovesLiquidityAndConflictsOnTerminalOrders(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 7, 99)
	cancelled, err := matching.CancelOrder("bid-1")
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if cancelled.Status != engine.StatusCancelled || cancelled.LastUpdatedBy != "cancelled" {
		t.Fatalf("unexpected cancelled order: %+v", cancelled)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 0 || book.BidCount != 0 {
		t.Fatalf("cancelled liquidity must leave the book: %+v", book)
	}
	if _, err := matching.CancelOrder("bid-1"); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("second cancel must conflict, got %v", err)
	}
	if _, err := matching.CancelOrder("nope"); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("unknown order must be not_found, got %v", err)
	}
}

func TestPartiallyFilledOrderKeepsCancelledStatus(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 10, 100)
	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 4, 100)
	cancelled, err := matching.CancelOrder("bid-1")
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if cancelled.Status != engine.StatusCancelled || cancelled.FilledQty != 4 || cancelled.RemainingQty != 6 {
		t.Fatalf("a partially filled order that is cancelled stays cancelled: %+v", cancelled)
	}
}

func TestValidationRejectsMalformedOrders(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 2)

	cases := []struct {
		name      string
		symbol    string
		side      string
		orderType string
		quantity  int64
		price     int64
	}{
		{"unknown symbol", "NOPE", engine.SideBuy, engine.TypeLimit, 1, 100},
		{"bad side", "ACME", "long", engine.TypeLimit, 1, 100},
		{"bad type", "ACME", engine.SideBuy, "stop", 1, 100},
		{"zero quantity", "ACME", engine.SideBuy, engine.TypeLimit, 0, 100},
		{"negative quantity", "ACME", engine.SideBuy, engine.TypeLimit, -5, 100},
		{"quantity above the cap", "ACME", engine.SideBuy, engine.TypeLimit, engine.MaxQuantity + 1, 100},
		{"limit without a price", "ACME", engine.SideBuy, engine.TypeLimit, 1, 0},
		{"price above the cap", "ACME", engine.SideBuy, engine.TypeLimit, 1, engine.MaxQuantity + 1},
		{"market with a price", "ACME", engine.SideBuy, engine.TypeMarket, 1, 100},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := matching.PlaceOrder("", testCase.symbol, testCase.side, testCase.orderType, testCase.quantity, testCase.price)
			if err == nil {
				t.Fatalf("expected a rejection")
			}
			if !errors.Is(err, engine.ErrValidation) && !errors.Is(err, engine.ErrNotFound) {
				t.Fatalf("unexpected error class: %v", err)
			}
		})
	}
	if _, err := matching.PlaceOrder("dup", "ACME", engine.SideBuy, engine.TypeLimit, 1, 100); err != nil {
		t.Fatalf("first order must be accepted: %v", err)
	}
	if _, err := matching.PlaceOrder("dup", "ACME", engine.SideBuy, engine.TypeLimit, 1, 100); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("duplicate order id must conflict, got %v", err)
	}
}

func TestInvariantsHoldAfterMixedWorkload(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 1)

	for i := 0; i < 30; i++ {
		price := int64(1000 + (i%7)*2)
		if i%3 == 0 {
			mustOrder(t, matching, "", "ACME", engine.SideSell, engine.TypeLimit, int64(1+i%5), price)
			continue
		}
		mustOrder(t, matching, "", "ACME", engine.SideBuy, engine.TypeLimit, int64(1+i%5), price)
	}
	mustOrder(t, matching, "sweep", "ACME", engine.SideBuy, engine.TypeMarket, 6, 0)
	mustOrder(t, matching, "cross", "ACME", engine.SideSell, engine.TypeLimit, 40, 900)

	book := bookOf(t, matching, "ACME")
	if book.Crossed {
		t.Fatalf("best bid must stay below best ask: %+v", book)
	}
	if book.BestBid != nil && book.BestAsk != nil && *book.BestBid >= *book.BestAsk {
		t.Fatalf("crossed book: %s >= %s", *book.BestBid, *book.BestAsk)
	}

	traded := map[string]int64{}
	page, err := matching.Trades("ACME", 0, 1000)
	if err != nil {
		t.Fatalf("Trades: %v", err)
	}
	for _, trade := range page.Trades {
		traded[trade.MakerOrder] += trade.Quantity
		traded[trade.TakerOrder] += trade.Quantity
	}
	for _, event := range matching.Events() {
		if event.Type != engine.EventOrderAccepted {
			continue
		}
		id, _ := event.Data["order_id"].(string)
		view, err := matching.GetOrder(id)
		if err != nil {
			t.Fatalf("GetOrder(%s): %v", id, err)
		}
		if view.FilledQty+view.RemainingQty != view.Quantity {
			t.Fatalf("order %s breaks quantity conservation: %+v", id, view)
		}
		if traded[id] != view.FilledQty {
			t.Fatalf("order %s traded %d but reports filled %d", id, traded[id], view.FilledQty)
		}
	}
}

func TestReplayRebuildsIdenticalState(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 2)

	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 30, 1005)
	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 10, 1010)
	mustOrder(t, matching, "bid-2", "ACME", engine.SideBuy, engine.TypeLimit, 10, 1010)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 5, 900)
	mustOrder(t, matching, "mkt-1", "ACME", engine.SideBuy, engine.TypeMarket, 50, 0)

	rebuilt, err := matching.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	live := matching.Events()
	if len(live) != len(rebuilt.Events) {
		t.Fatalf("event counts differ: live %d rebuilt %d", len(live), len(rebuilt.Events))
	}
	for i := range live {
		if live[i].Sequence != rebuilt.Events[i].Sequence {
			t.Fatalf("event %d sequence differs", i)
		}
		if live[i].Type != rebuilt.Events[i].Type {
			t.Fatalf("event %d type differs: %s vs %s", i, live[i].Type, rebuilt.Events[i].Type)
		}
	}
	if len(rebuilt.Books["ACME"].Index) != matching.CountOpenOrders("ACME") {
		t.Fatalf("rebuilt open order count differs")
	}
	view, _ := matching.Book("ACME", 100)
	if view.Crossed {
		t.Fatalf("rebuilt book must not be crossed")
	}
	page, _ := matching.Trades("ACME", 0, 1000)
	rebuiltTrades := 0
	for i := range rebuilt.Trades {
		if rebuilt.Trades[i].Symbol == "ACME" {
			rebuiltTrades++
		}
	}
	if page.Total != rebuiltTrades {
		t.Fatalf("rebuilt trades differ: live %d rebuilt %d", page.Total, rebuiltTrades)
	}
}

func TestReloadFromEventLogReproducesBook(t *testing.T) {
	directory := t.TempDir()
	matching, err := engine.NewEngine(directory, fixedClock())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	mustSymbol(t, matching, "ACME", 2)
	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 30, 1005)
	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 10, 1010)
	mustOrder(t, matching, "bid-2", "ACME", engine.SideBuy, engine.TypeLimit, 10, 1010)
	before := bookOf(t, matching, "ACME")
	beforeTrades, _ := matching.Trades("ACME", 0, 100)
	if err := matching.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reloaded, err := engine.NewEngine(directory, fixedClock())
	if err != nil {
		t.Fatalf("reload NewEngine: %v", err)
	}
	defer reloaded.Close()
	after := bookOf(t, reloaded, "ACME")
	afterTrades, _ := reloaded.Trades("ACME", 0, 100)
	if before.BestBid == nil || after.BestBid == nil || *before.BestBid != *after.BestBid {
		t.Fatalf("best bid differs after reload: %+v vs %+v", before, after)
	}
	if before.OpenOrders != after.OpenOrders || before.BidCount != after.BidCount {
		t.Fatalf("book shape differs after reload: %+v vs %+v", before, after)
	}
	if beforeTrades.Total != afterTrades.Total || len(afterTrades.Trades) != len(beforeTrades.Trades) {
		t.Fatalf("trade tape differs after reload")
	}
	for i := range beforeTrades.Trades {
		if beforeTrades.Trades[i] != afterTrades.Trades[i] {
			t.Fatalf("trade %d differs after reload", i)
		}
	}
	reloadedOrder, err := reloaded.GetOrder("bid-1")
	if err != nil {
		t.Fatalf("GetOrder after reload: %v", err)
	}
	originalOrder, _ := matching.GetOrder("bid-1")
	if reloadedOrder.FilledQty != originalOrder.FilledQty || reloadedOrder.RemainingQty != originalOrder.RemainingQty {
		t.Fatalf("order quantities differ after reload")
	}
}
