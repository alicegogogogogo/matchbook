package engine_test

import (
	"errors"
	"testing"

	"matchbook/internal/engine"
)

// settleIOCScenario seeds a two-level ask book shared by the ioc tests.
func seedAskBook(t *testing.T, matching *engine.Engine) {
	t.Helper()
	mustSymbol(t, matching, "ACME", 0)
	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 4, 103)
}

func TestIOCOrderSweepsAndCancelsRemainder(t *testing.T) {
	matching := newEngine(t)
	seedAskBook(t, matching)

	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 12, 103)
	if view.Status != engine.StatusCancelled {
		t.Fatalf("a partially filled ioc order must end cancelled: %+v", view)
	}
	if view.FilledQty != 10 || view.RemainingQty != 2 {
		t.Fatalf("ioc must report what traded and what did not: %+v", view)
	}
	if view.FilledQty+view.RemainingQty != view.Quantity {
		t.Fatalf("quantity must be conserved: %+v", view)
	}
	if view.LimitTicks == nil || *view.LimitTicks != 103 || view.LimitPrice == nil || *view.LimitPrice != "103" {
		t.Fatalf("ioc projection must carry its limit price: %+v", view)
	}
	if len(view.TradeIDs) != 2 {
		t.Fatalf("ioc must trade against every reachable level: %+v", view.TradeIDs)
	}

	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 2 {
		t.Fatalf("expected 2 trades, got %d", page.Total)
	}
	if page.Trades[0].PriceTicks != 100 || page.Trades[0].Quantity != 6 || page.Trades[0].MakerOrder != "ask-1" {
		t.Fatalf("best price trades first at the maker price: %+v", page.Trades[0])
	}
	if page.Trades[1].PriceTicks != 103 || page.Trades[1].Quantity != 4 || page.Trades[1].MakerOrder != "ask-2" {
		t.Fatalf("ioc must walk to the next level at the maker price: %+v", page.Trades[1])
	}

	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 0 || book.AskCount != 0 || book.BidCount != 0 || book.Crossed {
		t.Fatalf("an ioc remainder must never rest: %+v", book)
	}
	if _, err := matching.CancelOrder("ioc-1"); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("a cancelled ioc order is terminal, got %v", err)
	}
}

func TestIOCOrderFullyFilledIsFilledNotCancelled(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)
	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)

	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 3, 100)
	if view.Status != engine.StatusFilled || view.FilledQty != 3 || view.RemainingQty != 0 {
		t.Fatalf("a fully filled ioc order must be filled: %+v", view)
	}
	book := bookOf(t, matching, "ACME")
	if book.Asks[0].Quantity != 3 || book.Asks[0].OrderCount != 1 {
		t.Fatalf("the resting maker must keep its unfilled quantity: %+v", book.Asks)
	}
}

func TestIOCOrderWithNoLiquidityIsCancelledWithZeroFill(t *testing.T) {
	matching := newEngine(t)
	seedAskBook(t, matching)

	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 5, 99)
	if view.Status != engine.StatusCancelled || view.FilledQty != 0 || view.RemainingQty != 5 {
		t.Fatalf("an ioc order crossing nothing must be cancelled unfilled: %+v", view)
	}
	if len(view.TradeIDs) != 0 {
		t.Fatalf("a zero-fill ioc order must record no trades: %+v", view)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 0 {
		t.Fatalf("no trades may execute below the best ask, got %d", page.Total)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 2 || book.AskCount != 2 {
		t.Fatalf("a zero-fill ioc order must leave the book untouched: %+v", book)
	}
}

func TestIOCOrderRespectsLimitBoundary(t *testing.T) {
	matching := newEngine(t)
	seedAskBook(t, matching)

	// 6 trade at 100, the 103 level is outside the limit and stays resting.
	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 12, 100)
	if view.Status != engine.StatusCancelled || view.FilledQty != 6 || view.RemainingQty != 6 {
		t.Fatalf("ioc must stop at its limit price: %+v", view)
	}
	book := bookOf(t, matching, "ACME")
	if book.AskCount != 1 || book.Asks[0].Price != 103 || book.Asks[0].Quantity != 4 {
		t.Fatalf("the level beyond the limit must survive: %+v", book.Asks)
	}
}

func TestIOCSellOrderSweepsBidSide(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)
	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 5, 97)
	mustOrder(t, matching, "bid-2", "ACME", engine.SideBuy, engine.TypeLimit, 5, 95)

	view := mustOrder(t, matching, "ioc-s", "ACME", engine.SideSell, engine.TypeIOC, 12, 95)
	if view.Status != engine.StatusCancelled || view.FilledQty != 10 || view.RemainingQty != 2 {
		t.Fatalf("a sell ioc must sweep bids at or above its limit: %+v", view)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Trades[0].PriceTicks != 97 || page.Trades[1].PriceTicks != 95 {
		t.Fatalf("sell ioc must walk bids from best to worst: %+v", page.Trades)
	}
}

func TestFOKOrderFillsAcrossLevelsWithFIFO(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)
	mustOrder(t, matching, "ask-a", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)
	mustOrder(t, matching, "ask-b", "ACME", engine.SideSell, engine.TypeLimit, 4, 100)
	mustOrder(t, matching, "ask-c", "ACME", engine.SideSell, engine.TypeLimit, 5, 103)

	view := mustOrder(t, matching, "fok-1", "ACME", engine.SideBuy, engine.TypeFOK, 12, 103)
	if view.Status != engine.StatusFilled || view.FilledQty != 12 || view.RemainingQty != 0 {
		t.Fatalf("a fillable fok order must be fully filled: %+v", view)
	}
	if len(view.TradeIDs) != 3 {
		t.Fatalf("fok must consume both orders at 100 then part of 103: %+v", view.TradeIDs)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	want := []struct {
		price int64
		qty   int64
		maker string
	}{
		{100, 6, "ask-a"},
		{100, 4, "ask-b"},
		{103, 2, "ask-c"},
	}
	if page.Total != len(want) {
		t.Fatalf("expected %d trades, got %d", len(want), page.Total)
	}
	for i, fill := range want {
		got := page.Trades[i]
		if got.PriceTicks != fill.price || got.Quantity != fill.qty || got.MakerOrder != fill.maker {
			t.Fatalf("trade %d = %+v, want price %d qty %d maker %s", i, got, fill.price, fill.qty, fill.maker)
		}
		if got.TakerOrder != "fok-1" || got.MakerSide != engine.SideSell {
			t.Fatalf("trade %d must name the fok order as taker: %+v", i, got)
		}
	}
	book := bookOf(t, matching, "ACME")
	if book.AskCount != 1 || book.Asks[0].Price != 103 || book.Asks[0].Quantity != 3 || book.Asks[0].OrderCount != 1 {
		t.Fatalf("only the remainder of ask-c must rest: %+v", book.Asks)
	}
}

func TestFOKOrderRejectedWhenTotalLiquidityShort(t *testing.T) {
	matching := newEngine(t)
	seedAskBook(t, matching)

	view := mustOrder(t, matching, "fok-1", "ACME", engine.SideBuy, engine.TypeFOK, 11, 103)
	if view.Status != engine.StatusRejected {
		t.Fatalf("an unfillable fok order must be rejected: %+v", view)
	}
	if view.FilledQty != 0 || view.RemainingQty != 11 {
		t.Fatalf("a rejected fok order must show zero fill and full remainder: %+v", view)
	}
	if len(view.TradeIDs) != 0 {
		t.Fatalf("a rejected fok order must produce no trades at all: %+v", view.TradeIDs)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 0 {
		t.Fatalf("fok rejection must be atomic, but the tape holds %d trades", page.Total)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 2 || book.Asks[0].Quantity != 6 || book.Asks[1].Quantity != 4 {
		t.Fatalf("the book must be untouched after a fok rejection: %+v", book)
	}
	stored, err := matching.GetOrder("fok-1")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if stored.Status != engine.StatusRejected || stored.FilledQty != 0 || stored.RemainingQty != 11 {
		t.Fatalf("the rejection must persist in the order projection: %+v", stored)
	}
	if _, err := matching.CancelOrder("fok-1"); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("a rejected fok order is terminal, got %v", err)
	}
}

func TestFOKOrderRejectedWhenLiquidityBeyondLimit(t *testing.T) {
	matching := newEngine(t)
	seedAskBook(t, matching)

	// Ten units exist in total, but only six sit at or below the limit of 100.
	view := mustOrder(t, matching, "fok-1", "ACME", engine.SideBuy, engine.TypeFOK, 10, 100)
	if view.Status != engine.StatusRejected || view.FilledQty != 0 || view.RemainingQty != 10 {
		t.Fatalf("liquidity above the limit must not count toward a fok fill: %+v", view)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 0 {
		t.Fatalf("no trade may happen when the limit-scoped quantity is short, got %d", page.Total)
	}
	book := bookOf(t, matching, "ACME")
	if book.Asks[0].Quantity != 6 || book.Asks[1].Quantity != 4 {
		t.Fatalf("both levels must survive a fok rejection: %+v", book.Asks)
	}
}

func TestFOKOrderExactQuantityFills(t *testing.T) {
	matching := newEngine(t)
	seedAskBook(t, matching)

	view := mustOrder(t, matching, "fok-1", "ACME", engine.SideBuy, engine.TypeFOK, 10, 103)
	if view.Status != engine.StatusFilled || view.FilledQty != 10 || view.RemainingQty != 0 {
		t.Fatalf("a fok order matching the available quantity exactly must fill: %+v", view)
	}
	if book := bookOf(t, matching, "ACME"); book.OpenOrders != 0 {
		t.Fatalf("the book must be flat: %+v", book)
	}
}

func TestFOKSellOrderRejectedOnThinBidSide(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)
	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 5, 97)
	mustOrder(t, matching, "bid-2", "ACME", engine.SideBuy, engine.TypeLimit, 5, 95)

	rejected := mustOrder(t, matching, "fok-s", "ACME", engine.SideSell, engine.TypeFOK, 11, 95)
	if rejected.Status != engine.StatusRejected || rejected.FilledQty != 0 || rejected.RemainingQty != 11 {
		t.Fatalf("a sell fok short one unit must reject atomically: %+v", rejected)
	}
	filled := mustOrder(t, matching, "fok-s2", "ACME", engine.SideSell, engine.TypeFOK, 10, 95)
	if filled.Status != engine.StatusFilled || filled.FilledQty != 10 {
		t.Fatalf("a sell fok with full bid support must fill: %+v", filled)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 2 || page.Trades[0].PriceTicks != 97 || page.Trades[1].PriceTicks != 95 {
		t.Fatalf("only the successful fok must appear on the tape: %+v", page.Trades)
	}
}

func TestIOCAndFOKValidation(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 2)

	cases := []struct {
		name      string
		orderType string
		price     int64
	}{
		{"ioc without a price", engine.TypeIOC, 0},
		{"fok without a price", engine.TypeFOK, 0},
		{"ioc with negative price", engine.TypeIOC, -1},
		{"fok with negative price", engine.TypeFOK, -1},
		{"ioc price above cap", engine.TypeIOC, engine.MaxQuantity + 1},
		{"unknown type", "stop", 100},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := matching.PlaceOrder("", "ACME", engine.SideBuy, testCase.orderType, 1, testCase.price)
			if !errors.Is(err, engine.ErrValidation) {
				t.Fatalf("expected validation_error, got %v", err)
			}
		})
	}
	if _, err := matching.PlaceOrder("", "ACME", engine.SideBuy, engine.TypeMarket, 1, 100); !errors.Is(err, engine.ErrValidation) {
		t.Fatalf("market orders must still reject a limit_price, got %v", err)
	}
	if _, err := matching.PlaceOrder("", "NOPE", engine.SideBuy, engine.TypeIOC, 1, 100); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("unknown symbol must be not_found, got %v", err)
	}

	mustOrder(t, matching, "dup", "ACME", engine.SideBuy, engine.TypeFOK, 1, 100)
	if _, err := matching.PlaceOrder("dup", "ACME", engine.SideBuy, engine.TypeFOK, 1, 100); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("duplicate fok order id must conflict, got %v", err)
	}
}

func TestIOCAndFOKRebuildIdentically(t *testing.T) {
	directory := t.TempDir()
	matching, err := engine.NewEngine(directory, fixedClock())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	mustSymbol(t, matching, "ACME", 2)

	// Resting liquidity on both sides; the 990 bid never crosses asks at 1005+.
	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 1005)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 4, 1010)
	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 3, 990)
	mustOrder(t, matching, "bid-2", "ACME", engine.SideBuy, engine.TypeLimit, 2, 900)

	// ioc: 10 trade across both asks, 5 cancelled.
	iocView := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 15, 1010)
	if iocView.Status != engine.StatusCancelled || iocView.FilledQty != 10 || iocView.RemainingQty != 5 {
		t.Fatalf("seed ioc setup wrong: %+v", iocView)
	}
	// fok: the ask side is now empty, so this rejects with zero trades.
	fokRejected := mustOrder(t, matching, "fok-x", "ACME", engine.SideBuy, engine.TypeFOK, 1, 1005)
	if fokRejected.Status != engine.StatusRejected || fokRejected.FilledQty != 0 {
		t.Fatalf("seed fok rejection setup wrong: %+v", fokRejected)
	}
	// Two makers share one level; a fok must consume both in FIFO order.
	mustOrder(t, matching, "ask-3", "ACME", engine.SideSell, engine.TypeLimit, 2, 1008)
	mustOrder(t, matching, "ask-4", "ACME", engine.SideSell, engine.TypeLimit, 3, 1008)
	fokFilled := mustOrder(t, matching, "fok-ok", "ACME", engine.SideBuy, engine.TypeFOK, 5, 1008)
	if fokFilled.Status != engine.StatusFilled || fokFilled.FilledQty != 5 {
		t.Fatalf("seed fok fill setup wrong: %+v", fokFilled)
	}
	// Sell-side ioc takes the 3-unit bid at 990, leaves its last unit cancelled;
	// the 900 bid is outside the limit and keeps resting.
	sellIOC := mustOrder(t, matching, "ioc-s", "ACME", engine.SideSell, engine.TypeIOC, 4, 990)
	if sellIOC.Status != engine.StatusCancelled || sellIOC.FilledQty != 3 || sellIOC.RemainingQty != 1 {
		t.Fatalf("seed sell ioc setup wrong: %+v", sellIOC)
	}

	liveEvents := matching.Events()
	liveTrades, _ := matching.Trades("ACME", 0, 1000)
	liveBook := bookOf(t, matching, "ACME")

	rebuilt, err := matching.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(rebuilt.Events) != len(liveEvents) {
		t.Fatalf("event counts differ: live %d rebuilt %d", len(liveEvents), len(rebuilt.Events))
	}
	for i := range liveEvents {
		if liveEvents[i].Type != rebuilt.Events[i].Type || liveEvents[i].Sequence != rebuilt.Events[i].Sequence {
			t.Fatalf("event %d differs: %+v vs %+v", i, liveEvents[i], rebuilt.Events[i])
		}
	}

	// Every order, terminal or resting, must rebuild to the same projection.
	for _, event := range liveEvents {
		if event.Type != engine.EventOrderAccepted {
			continue
		}
		id, _ := event.Data["order_id"].(string)
		liveOrder, err := matching.GetOrder(id)
		if err != nil {
			t.Fatalf("live GetOrder(%s): %v", id, err)
		}
		rebuiltOrder, ok := rebuilt.Orders[id]
		if !ok {
			t.Fatalf("rebuilt state is missing order %s", id)
		}
		if rebuiltOrder.Status != liveOrder.Status ||
			rebuiltOrder.Filled != liveOrder.FilledQty ||
			rebuiltOrder.Remaining != liveOrder.RemainingQty ||
			rebuiltOrder.Quantity != liveOrder.Quantity ||
			rebuiltOrder.Type != liveOrder.Type ||
			rebuiltOrder.LimitPrice != derefTicks(liveOrder.LimitTicks) {
			t.Fatalf("order %s rebuild mismatch: live %+v rebuilt %+v", id, liveOrder, rebuiltOrder)
		}
	}

	// The cancelled ioc and zero-fill rejected fok must not rest after replay.
	if len(rebuilt.Books["ACME"].Index) != liveBook.OpenOrders {
		t.Fatalf("rebuilt open order count %d != live %d", len(rebuilt.Books["ACME"].Index), liveBook.OpenOrders)
	}
	for _, id := range []string{"ioc-1", "fok-x", "fok-ok", "ioc-s"} {
		if _, resting := rebuilt.Books["ACME"].Index[id]; resting {
			t.Fatalf("terminal order %s must not rest in the rebuilt book", id)
		}
	}

	// Trade order, quantities, prices and sides must match tick for tick.
	rebuiltTrades := 0
	for i := range rebuilt.Trades {
		if rebuilt.Trades[i].Symbol != "ACME" {
			continue
		}
		if rebuiltTrades >= len(liveTrades.Trades) {
			t.Fatalf("rebuilt has more trades than live")
		}
		live := liveTrades.Trades[rebuiltTrades]
		got := rebuilt.Trades[i]
		if got.ID != live.ID || got.Price != live.PriceTicks || got.Quantity != live.Quantity ||
			got.MakerID != live.MakerOrder || got.TakerID != live.TakerOrder || got.MakerSide != live.MakerSide {
			t.Fatalf("rebuilt trade %d differs: live %+v rebuilt %+v", rebuiltTrades, live, got)
		}
		rebuiltTrades++
	}
	if rebuiltTrades != liveTrades.Total {
		t.Fatalf("rebuilt trade count %d != live %d", rebuiltTrades, liveTrades.Total)
	}

	// Price levels must match level for level.
	rebuiltBids, rebuiltAsks := rebuilt.Books["ACME"].Snapshot(100)
	assertLevelsEqual(t, "bid", liveBook.Bids, rebuiltBids)
	assertLevelsEqual(t, "ask", liveBook.Asks, rebuiltAsks)

	// And after a full restart from disk the same terminal states come back.
	if err := matching.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reloaded, err := engine.NewEngine(directory, fixedClock())
	if err != nil {
		t.Fatalf("reload NewEngine: %v", err)
	}
	defer reloaded.Close()
	for _, check := range []struct {
		id     string
		status string
		filled int64
		left   int64
	}{
		{"ioc-1", engine.StatusCancelled, 10, 5},
		{"fok-x", engine.StatusRejected, 0, 1},
		{"fok-ok", engine.StatusFilled, 5, 0},
		{"ioc-s", engine.StatusCancelled, 3, 1},
	} {
		view, err := reloaded.GetOrder(check.id)
		if err != nil {
			t.Fatalf("GetOrder(%s) after reload: %v", check.id, err)
		}
		if view.Status != check.status || view.FilledQty != check.filled || view.RemainingQty != check.left {
			t.Fatalf("order %s after reload = %+v, want status %s filled %d remaining %d",
				check.id, view, check.status, check.filled, check.left)
		}
	}
	reloadedBook := bookOf(t, reloaded, "ACME")
	if reloadedBook.OpenOrders != liveBook.OpenOrders || reloadedBook.AskCount != liveBook.AskCount || reloadedBook.BidCount != liveBook.BidCount {
		t.Fatalf("book shape differs after reload: %+v vs %+v", reloadedBook, liveBook)
	}
	reloadedTrades, _ := reloaded.Trades("ACME", 0, 1000)
	if reloadedTrades.Total != liveTrades.Total {
		t.Fatalf("trade tape length differs after reload: %d vs %d", reloadedTrades.Total, liveTrades.Total)
	}
	for i := range liveTrades.Trades {
		live := liveTrades.Trades[i]
		got := reloadedTrades.Trades[i]
		if got.ID != live.ID || got.PriceTicks != live.PriceTicks || got.Quantity != live.Quantity ||
			got.MakerOrder != live.MakerOrder || got.TakerOrder != live.TakerOrder {
			t.Fatalf("trade %d differs after reload: %+v vs %+v", i, got, live)
		}
	}
}

func derefTicks(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func assertLevelsEqual(t *testing.T, side string, live, rebuilt []engine.Level) {
	t.Helper()
	if len(live) != len(rebuilt) {
		t.Fatalf("%s level counts differ: live %+v rebuilt %+v", side, live, rebuilt)
	}
	for i := range live {
		if live[i] != rebuilt[i] {
			t.Fatalf("%s level %d differs: live %+v rebuilt %+v", side, i, live[i], rebuilt[i])
		}
	}
}
