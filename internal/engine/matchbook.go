package engine

import "fmt"

// Order and trade lifecycle vocabulary.
const (
	SideBuy  = "buy"
	SideSell = "sell"

	TypeLimit  = "limit"
	TypeMarket = "market"
	TypeIOC    = "ioc"
	TypeFOK    = "fok"

	StatusPending   = "pending"
	StatusOpen      = "open"
	StatusFilled    = "filled"
	StatusCancelled = "cancelled"
	StatusRejected  = "rejected"
)

// Event types recorded in the append-only log. Replay understands only these.
const (
	EventSymbolAdded    = "symbol_added"
	EventOrderAccepted  = "order_accepted"
	EventOrderUpdated   = "order_updated"
	EventOrderFilled    = "order_filled"
	EventOrderCancelled = "order_cancelled"
	EventOrderRejected  = "order_rejected"
	EventTradeExecuted  = "trade_executed"
)

// MaxQuantity is the largest accepted order quantity.
const MaxQuantity int64 = 1_000_000_000

// Symbol is a tradable instrument plus the scaling convention used to talk
// about its prices.
//
// Prices and quantities are always exchanged as integers. A price of p means
// p / 10^PriceScale quote units per base unit, so no floating point value ever
// reaches the matching engine.
type Symbol struct {
	Name       string `json:"name"`
	PriceScale int    `json:"price_scale"`
}

// Order is a single order. Filled and Remaining are maintained so that
// Quantity == Filled + Remaining holds at every observable point in time.
type Order struct {
	ID         string
	Symbol     string
	Side       string
	Type       string
	LimitPrice int64 // 0 for market orders
	Quantity   int64
	Filled     int64
	Remaining  int64
	Status     string
	Trades     []string
	Sequence   int64
	CreatedAt  int64
	UpdatedAt  int64
}

// Trade is one execution between a resting (maker) order and an incoming
// (taker) order. Price is always the maker's price.
type Trade struct {
	ID         string `json:"id"`
	Sequence   int64  `json:"sequence"`
	Symbol     string `json:"symbol"`
	Price      int64  `json:"price"`
	Quantity   int64  `json:"quantity"`
	MakerID    string `json:"maker_order_id"`
	TakerID    string `json:"taker_order_id"`
	MakerSide  string `json:"maker_side"`
	ExecutedAt int64  `json:"executed_at"`
}

// Level is one aggregated price level of a book snapshot.
type Level struct {
	Price      int64 `json:"price"`
	Quantity   int64 `json:"quantity"`
	OrderCount int   `json:"order_count"`
}

// OrderView is the public projection of an Order, with prices already scaled
// for display.
type OrderView struct {
	ID            string   `json:"id"`
	Symbol        string   `json:"symbol"`
	Side          string   `json:"side"`
	Type          string   `json:"type"`
	Status        string   `json:"status"`
	PriceScale    int      `json:"price_scale"`
	LimitPrice    *string  `json:"limit_price"`
	LimitTicks    *int64   `json:"limit_price_ticks"`
	Quantity      int64    `json:"quantity"`
	FilledQty     int64    `json:"filled_quantity"`
	RemainingQty  int64    `json:"remaining_quantity"`
	TradeIDs      []string `json:"trade_ids"`
	Sequence      int64    `json:"sequence"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
	LastUpdatedBy string   `json:"last_updated_by"`
}

// TradeView is the public projection of a Trade with scaled prices.
type TradeView struct {
	ID         string `json:"id"`
	Sequence   int64  `json:"sequence"`
	Symbol     string `json:"symbol"`
	PriceScale int    `json:"price_scale"`
	Price      string `json:"price"`
	PriceTicks int64  `json:"price_ticks"`
	Quantity   int64  `json:"quantity"`
	MakerOrder string `json:"maker_order_id"`
	TakerOrder string `json:"taker_order_id"`
	MakerSide  string `json:"maker_side"`
	ExecutedAt int64  `json:"executed_at"`
}

// ScalePrice renders integer ticks as a decimal string using the symbol's
// price scale. The result is exact and stable for a given scale.
func ScalePrice(ticks int64, scale int) string {
	if scale <= 0 {
		return fmt.Sprintf("%d", ticks)
	}
	sign := ""
	if ticks < 0 {
		sign = "-"
		ticks = -ticks
	}
	unit := int64(1)
	for i := 0; i < scale; i++ {
		unit *= 10
	}
	return fmt.Sprintf("%s%d.%0*d", sign, ticks/unit, scale, ticks%unit)
}

// orderView projects an order for the API, using "created" or "updated" as the
// reason the order was last written.
func orderView(order *Order, symbol *Symbol, reason string) OrderView {
	view := OrderView{
		ID:            order.ID,
		Symbol:        order.Symbol,
		Side:          order.Side,
		Type:          order.Type,
		Status:        order.Status,
		PriceScale:    symbol.PriceScale,
		Quantity:      order.Quantity,
		FilledQty:     order.Filled,
		RemainingQty:  order.Remaining,
		TradeIDs:      append([]string{}, order.Trades...),
		Sequence:      order.Sequence,
		CreatedAt:     order.CreatedAt,
		UpdatedAt:     order.UpdatedAt,
		LastUpdatedBy: reason,
	}
	if order.Type == TypeLimit || order.Type == TypeIOC || order.Type == TypeFOK {
		price := ScalePrice(order.LimitPrice, symbol.PriceScale)
		ticks := order.LimitPrice
		view.LimitPrice = &price
		view.LimitTicks = &ticks
	}
	return view
}

// tradeView projects a trade for the API.
func tradeView(trade *Trade, symbol *Symbol) TradeView {
	return TradeView{
		ID:         trade.ID,
		Sequence:   trade.Sequence,
		Symbol:     trade.Symbol,
		PriceScale: symbol.PriceScale,
		Price:      ScalePrice(trade.Price, symbol.PriceScale),
		PriceTicks: trade.Price,
		Quantity:   trade.Quantity,
		MakerOrder: trade.MakerID,
		TakerOrder: trade.TakerID,
		MakerSide:  trade.MakerSide,
		ExecutedAt: trade.ExecutedAt,
	}
}

// levelViews converts a snapshot to the public level shape.
func levelViews(levels []Level) []Level {
	if levels == nil {
		return []Level{}
	}
	return levels
}
