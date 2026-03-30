package main

import (
	"math"
	"runtime"
	"testing"
)

// ─── Helpers ─────────────────────────────────────────────────────────────────

func defaultCfg() Config {
	return Config{
		ATRPeriod:     5,
		ATRMultiplier: 2.0,
		SMAPeriod:     5,
		GridCount:     4,
		QuoteQty:      100.0,
		FeeRate:       0.001,
		InitialUSDT:   1000.0,
		TrailingStep:  0.05,
	}
}

func makeCandle(close float64) Candle {
	return Candle{Open: close, High: close + 1, Low: close - 1, Close: close}
}

func makeCandles(closes []float64) []Candle {
	candles := make([]Candle, len(closes))
	for i, c := range closes {
		candles[i] = Candle{
			Open:  c,
			High:  c + 5,
			Low:   c - 5,
			Close: c,
		}
	}
	return candles
}

// ─── calculateSMA ─────────────────────────────────────────────────────────────

func TestCalculateSMA_NotEnoughCandles(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{100, 200, 300}) // SMAPeriod=5, only 3 candles
	if sma := e.calculateSMA(); sma != 0 {
		t.Errorf("expected 0, got %f", sma)
	}
}

func TestCalculateSMA_Correct(t *testing.T) {
	e := NewEngine(defaultCfg())
	// SMAPeriod=5, last 5 values: 10,20,30,40,50 → avg=30
	e.candles = makeCandles([]float64{1, 2, 10, 20, 30, 40, 50})
	got := e.calculateSMA()
	want := 30.0
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("expected %.2f, got %.2f", want, got)
	}
}

// ─── calculateATR ─────────────────────────────────────────────────────────────

func TestCalculateATR_NotEnoughCandles(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{100, 200}) // ATRPeriod=5
	if atr := e.calculateATR(); atr != 0 {
		t.Errorf("expected 0, got %f", atr)
	}
}

func TestCalculateATR_Positive(t *testing.T) {
	e := NewEngine(defaultCfg())
	// ATRPeriod=5: need > 5 candles
	closes := []float64{100, 110, 105, 115, 108, 120}
	candles := make([]Candle, len(closes))
	for i, c := range closes {
		candles[i] = Candle{Open: c, High: c + 3, Low: c - 3, Close: c}
	}
	e.candles = candles
	atr := e.calculateATR()
	if atr <= 0 {
		t.Errorf("expected positive ATR, got %f", atr)
	}
}

// ─── exitToCash ───────────────────────────────────────────────────────────────

func TestExitToCash_SellsBTC(t *testing.T) {
	e := NewEngine(defaultCfg())
	// Add enough candles so calculateSMA doesn't panic
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.btcBal = 0.5
	e.usdtBal = 100.0
	e.inMarket = true

	price := 1000.0
	e.exitToCash(price)

	wantUSDT := 100.0 + 0.5*1000.0*(1-0.001)
	if math.Abs(e.usdtBal-wantUSDT) > 1e-6 {
		t.Errorf("expected USDT %.6f, got %.6f", wantUSDT, e.usdtBal)
	}
	if e.btcBal != 0 {
		t.Errorf("expected btcBal=0, got %f", e.btcBal)
	}
	if e.inMarket {
		t.Error("expected inMarket=false")
	}
	if e.trades != 1 {
		t.Errorf("expected 1 trade, got %d", e.trades)
	}
}

func TestExitToCash_NoBTC(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.btcBal = 0
	e.usdtBal = 500.0
	e.inMarket = true

	e.exitToCash(1000.0)

	if e.usdtBal != 500.0 {
		t.Errorf("USDT should be unchanged, got %f", e.usdtBal)
	}
	if e.trades != 0 {
		t.Errorf("no trade should happen, got %d", e.trades)
	}
}

// ─── setupGrid ────────────────────────────────────────────────────────────────

func TestSetupGrid_LevelCount(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.setupGrid(1000.0, 50.0)

	if len(e.levels) != e.cfg.GridCount+1 {
		t.Errorf("expected %d levels, got %d", e.cfg.GridCount+1, len(e.levels))
	}
}

func TestSetupGrid_SetsInMarket(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.setupGrid(1000.0, 50.0)

	if !e.inMarket {
		t.Error("expected inMarket=true after setupGrid")
	}
	if e.lastCenter != 1000.0 {
		t.Errorf("expected lastCenter=1000, got %f", e.lastCenter)
	}
}

func TestSetupGrid_MinDynamicRange(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	// Very small ATR → dynamicRange should be clamped to 0.03
	e.setupGrid(1000.0, 0.001)

	// With 3% range around 1000: low = 985, high = 1015
	if e.levels[0] < 984 || e.levels[0] > 986 {
		t.Errorf("unexpected bottom level: %f", e.levels[0])
	}
}

func TestSetupGrid_BuyOrdersBelowPrice(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.setupGrid(1000.0, 50.0)

	for i, ord := range e.orders {
		if e.levels[i] < 1000.0 && ord.Side != 1 {
			t.Errorf("level %d (%.2f) below price should be BUY, got side=%d", i, e.levels[i], ord.Side)
		}
	}
}

// ─── executeOrder ─────────────────────────────────────────────────────────────

func TestExecuteOrder_Buy(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.setupGrid(1000.0, 50.0)

	// Find a BUY order
	var buyIdx int = -1
	for i, ord := range e.orders {
		if ord.Side == 1 && ord.Active {
			buyIdx = i
			break
		}
	}
	if buyIdx == -1 {
		t.Skip("no active BUY orders after setupGrid")
	}

	initialUSDT := e.usdtBal
	price := e.levels[buyIdx]
	e.executeOrder(buyIdx, price)

	if e.usdtBal >= initialUSDT {
		t.Error("USDT should decrease after BUY")
	}
	if e.btcBal <= 0 {
		t.Error("BTC should increase after BUY")
	}
	if e.trades != 1 {
		t.Errorf("expected 1 trade, got %d", e.trades)
	}
	if e.orders[buyIdx].Active {
		t.Error("order should be deactivated after execution")
	}
}

func TestExecuteOrder_InsufficientUSDT(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.usdtBal = 0 // no funds
	e.setupGrid(1000.0, 50.0)

	for i, ord := range e.orders {
		if ord.Side == 1 && ord.Active {
			e.executeOrder(i, e.levels[i])
			if e.trades != 0 {
				t.Error("trade should not execute without USDT")
			}
			break
		}
	}
}

func TestExecuteOrder_InactiveOrder(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.levels = []float64{990.0, 1010.0}
	e.orders[0] = &Order{Price: 990.0, Side: 1, Active: false}

	e.executeOrder(0, 990.0)

	if e.trades != 0 {
		t.Error("inactive order should not execute")
	}
}

func TestExecuteOrder_MirrorOrderCreated(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})
	e.setupGrid(1000.0, 50.0)

	var buyIdx int = -1
	for i, ord := range e.orders {
		if ord.Side == 1 && ord.Active && i > 0 {
			buyIdx = i
			break
		}
	}
	if buyIdx == -1 {
		t.Skip("no suitable BUY order found")
	}

	e.executeOrder(buyIdx, e.levels[buyIdx])

	mirrorIdx := buyIdx - 1 // BUY side=-1 → mirror = idx - 1
	if mir, ok := e.orders[mirrorIdx]; ok {
		if mir.Side != -1 {
			t.Errorf("mirror order should be SELL (-1), got %d", mir.Side)
		}
	}
}

// ─── AddCandle ────────────────────────────────────────────────────────────────

func TestAddCandle_BufferCap(t *testing.T) {
	e := NewEngine(defaultCfg())
	for i := 0; i < CandleLimit+10; i++ {
		e.AddCandle(makeCandle(float64(100 + i)))
	}
	if len(e.candles) > CandleLimit {
		t.Errorf("candles buffer exceeded %d: got %d", CandleLimit, len(e.candles))
	}
}

func TestAddCandle_NotEnoughHistory_NoGrid(t *testing.T) {
	e := NewEngine(defaultCfg())
	// SMAPeriod=5, add only 3 candles — engine should not enter market
	for i := 0; i < 3; i++ {
		e.AddCandle(makeCandle(200.0))
	}
	if e.inMarket {
		t.Error("should not be in market with insufficient history")
	}
}

func TestAddCandle_BearishExitsMarket(t *testing.T) {
	e := NewEngine(defaultCfg())
	// Build history: 5 high candles (SMA ~200), then price drops below SMA
	for i := 0; i < 6; i++ {
		e.AddCandle(Candle{Open: 200, High: 205, Low: 195, Close: 200})
	}
	e.inMarket = true
	e.btcBal = 0.1

	// Candle with close below SMA
	e.AddCandle(Candle{Open: 100, High: 105, Low: 95, Close: 100})

	if e.inMarket {
		t.Error("should exit market when price < SMA")
	}
}

func TestAddCandle_BullishEntersMarket(t *testing.T) {
	e := NewEngine(defaultCfg())
	// 6 candles with close=100, SMA=100; then candle with close=200 > SMA
	for i := 0; i < 6; i++ {
		e.AddCandle(Candle{Open: 100, High: 105, Low: 95, Close: 100})
	}

	// Candle clearly above SMA
	e.AddCandle(Candle{Open: 200, High: 205, Low: 195, Close: 200})

	if !e.inMarket {
		t.Error("should enter market when price > SMA")
	}
}

func TestAddCandle_TrailingResetGrid(t *testing.T) {
	e := NewEngine(defaultCfg())
	// Enough history
	for i := 0; i < 6; i++ {
		e.AddCandle(Candle{Open: 100, High: 105, Low: 95, Close: 100})
	}
	// First bullish candle — sets grid at 200
	e.AddCandle(Candle{Open: 200, High: 205, Low: 195, Close: 200})
	center1 := e.lastCenter

	// Price moves > TrailingStep (5%) from center
	newPrice := center1 * 1.10
	e.AddCandle(Candle{Open: newPrice, High: newPrice + 5, Low: newPrice - 5, Close: newPrice})

	if e.lastCenter == center1 {
		t.Error("grid should reset after price moves beyond TrailingStep")
	}
}

// ─── Slippage ─────────────────────────────────────────────────────────────────

// Проскальзывание: цена исполнения значительно хуже, чем в ордере.
// Движок принимает переданную цену как фактическую, поэтому баланс должен
// отражать именно её, а не ord.Price.
func TestExecuteOrder_Slippage_BuyWorsePrice(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})

	orderPrice := 950.0
	slippagePrice := 980.0 // реальное исполнение хуже на 3%

	e.levels = []float64{orderPrice, 1050.0}
	e.orders[0] = &Order{Price: orderPrice, Side: 1, Active: true}

	initialUSDT := e.usdtBal
	e.executeOrder(0, slippagePrice)

	// Куплено BTC по slippagePrice, а не по orderPrice
	expectedBTC := (e.cfg.QuoteQty / slippagePrice) * (1 - e.cfg.FeeRate)
	if math.Abs(e.btcBal-expectedBTC) > 1e-9 {
		t.Errorf("BTC при проскальзывании: ожидалось %.9f, получено %.9f", expectedBTC, e.btcBal)
	}

	expectedUSDT := initialUSDT - e.cfg.QuoteQty
	if math.Abs(e.usdtBal-expectedUSDT) > 1e-9 {
		t.Errorf("USDT при проскальзывании: ожидалось %.6f, получено %.6f", expectedUSDT, e.usdtBal)
	}
}

func TestExecuteOrder_Slippage_SellWorsePrice(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.candles = makeCandles([]float64{90, 91, 92, 93, 94})

	orderPrice := 1050.0
	slippagePrice := 1010.0 // реальное исполнение хуже на ~4%

	e.levels = []float64{950.0, orderPrice}
	e.btcBal = 1.0
	e.orders[1] = &Order{Price: orderPrice, Side: -1, Active: true}

	initialUSDT := e.usdtBal
	e.executeOrder(1, slippagePrice)

	qtyToSell := e.cfg.QuoteQty / slippagePrice
	expectedUSDT := initialUSDT + qtyToSell*slippagePrice*(1-e.cfg.FeeRate)
	if math.Abs(e.usdtBal-expectedUSDT) > 1e-6 {
		t.Errorf("USDT при проскальзывании SELL: ожидалось %.6f, получено %.6f", expectedUSDT, e.usdtBal)
	}
}

// ─── Partial Fill / Alert ─────────────────────────────────────────────────────

// SELL пропускается (а не исполняется частично) когда btcBal < QuoteQty/price.
// Поведение симметрично BUY guard. OnAlert должен сработать с AlertInsufficientBTC.
func TestExecuteOrder_PartialFill_SellSkipsAndAlerts(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.levels = []float64{900.0, 1000.0, 1100.0}
	e.btcBal = 0.00001 // намеренно меньше QuoteQty/price = 100/1000 = 0.1
	e.orders[1] = &Order{Price: 1000.0, Side: -1, Active: true}

	var got *Alert
	e.OnAlert = func(a Alert) { got = &a }

	initialUSDT := e.usdtBal
	e.executeOrder(1, 1000.0)

	// Сделка не должна произойти
	if e.trades != 0 {
		t.Errorf("ожидалось 0 сделок, получено %d", e.trades)
	}
	if e.usdtBal != initialUSDT {
		t.Errorf("USDT не должен изменяться, получено %.6f", e.usdtBal)
	}
	if e.btcBal != 0.00001 {
		t.Errorf("BTC не должен изменяться, получено %.9f", e.btcBal)
	}
	// Зеркальный ордер не должен быть создан — нет overtrade
	if _, ok := e.orders[2]; ok {
		t.Error("зеркальный BUY не должен создаваться при отклонённом SELL")
	}
	// Alert должен сработать
	if got == nil {
		t.Fatal("OnAlert не был вызван")
	}
	if got.Kind != AlertInsufficientBTC {
		t.Errorf("ожидался AlertInsufficientBTC, получено %q", got.Kind)
	}
	if got.Have != 0.00001 {
		t.Errorf("Alert.Have=%.9f, ожидалось 0.000010000", got.Have)
	}
	if math.Abs(got.Need-0.1) > 1e-9 {
		t.Errorf("Alert.Need=%.9f, ожидалось 0.100000000", got.Need)
	}
}

// BUY guard тоже должен стрелять Alert с AlertInsufficientUSDT.
func TestExecuteOrder_Alert_InsufficientUSDT(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.levels = []float64{900.0, 1000.0}
	e.usdtBal = 0 // пусто
	e.orders[0] = &Order{Price: 900.0, Side: 1, Active: true}

	var got *Alert
	e.OnAlert = func(a Alert) { got = &a }

	e.executeOrder(0, 900.0)

	if e.trades != 0 {
		t.Errorf("ожидалось 0 сделок, получено %d", e.trades)
	}
	if got == nil {
		t.Fatal("OnAlert не был вызван")
	}
	if got.Kind != AlertInsufficientUSDT {
		t.Errorf("ожидался AlertInsufficientUSDT, получено %q", got.Kind)
	}
	if got.Have != 0 {
		t.Errorf("Alert.Have=%.2f, ожидалось 0", got.Have)
	}
	if got.Need != e.cfg.QuoteQty {
		t.Errorf("Alert.Need=%.2f, ожидалось QuoteQty=%.2f", got.Need, e.cfg.QuoteQty)
	}
}

// OnAlert=nil не должен вызывать панику (тихий режим по умолчанию).
func TestExecuteOrder_Alert_NilHandlerNoPanic(t *testing.T) {
	e := NewEngine(defaultCfg())
	e.levels = []float64{900.0, 1000.0}
	e.usdtBal = 0
	e.orders[0] = &Order{Price: 900.0, Side: 1, Active: true}
	// e.OnAlert == nil — должно работать без паники

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("паника при nil OnAlert: %v", r)
		}
	}()
	e.executeOrder(0, 900.0)
}

// SELL исполняется корректно, когда btcBal >= QuoteQty/price (guard не срабатывает).
func TestExecuteOrder_Sell_SufficientBTC_Executes(t *testing.T) {
	e := NewEngine(defaultCfg())
	price := 1000.0
	e.levels = []float64{900.0, price, 1100.0}
	e.btcBal = 1.0 // >> QuoteQty/price = 0.1
	e.orders[1] = &Order{Price: price, Side: -1, Active: true}

	var alerted bool
	e.OnAlert = func(Alert) { alerted = true }

	initialUSDT := e.usdtBal
	e.executeOrder(1, price)

	if alerted {
		t.Error("Alert не должен срабатывать при достаточном btcBal")
	}
	if e.trades != 1 {
		t.Errorf("ожидалась 1 сделка, получено %d", e.trades)
	}
	qtyToSell := e.cfg.QuoteQty / price
	expectedUSDT := initialUSDT + qtyToSell*price*(1-e.cfg.FeeRate)
	if math.Abs(e.usdtBal-expectedUSDT) > 1e-9 {
		t.Errorf("USDT после SELL: ожидалось %.9f, получено %.9f", expectedUSDT, e.usdtBal)
	}
}

// ─── Stress Test ──────────────────────────────────────────────────────────────

// Стресс-тест: 10 000 свечей подряд.
// Буфер не должен расти сверх CandleLimit, а прирост памяти — быть минимальным.
func TestAddCandle_StressBuffer(t *testing.T) {
	e := NewEngine(defaultCfg())

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	for i := 0; i < 10_000; i++ {
		price := 100.0 + float64(i%200) // цена колеблется, не растёт линейно
		e.AddCandle(Candle{Open: price, High: price + 2, Low: price - 2, Close: price})
	}

	runtime.GC()
	runtime.ReadMemStats(&memAfter)

	if len(e.candles) > CandleLimit {
		t.Errorf("буфер превысил CandleLimit=%d: len=%d", CandleLimit, len(e.candles))
	}

	// Прирост heap не должен превышать 10 МБ
	const maxGrowthBytes = 10 << 20
	if memAfter.HeapAlloc > memBefore.HeapAlloc+maxGrowthBytes {
		t.Errorf("утечка памяти: heap вырос на %d байт (лимит %d)",
			memAfter.HeapAlloc-memBefore.HeapAlloc, maxGrowthBytes)
	}
}

// ─── Trend Reversal Equity ────────────────────────────────────────────────────

// Смена тренда: цена сначала падает через всю сетку BUY (скупаем BTC),
// затем резко растёт через всю сетку SELL (продаём BTC).
// Equity (USDT + BTC*lastPrice) должен быть > 0 и не уходить в минус.
func TestTrendReversal_EquityConsistency(t *testing.T) {
	cfg := defaultCfg()
	cfg.GridCount = 6
	cfg.QuoteQty = 50.0
	cfg.InitialUSDT = 1000.0
	e := NewEngine(cfg)

	// Разогрев: 8 бычьих свечей вокруг 500, чтобы SMA=500
	for i := 0; i < 8; i++ {
		e.AddCandle(Candle{Open: 500, High: 510, Low: 490, Close: 500})
	}

	// Фаза 1: цена падает с 500 до 440 — широкий High/Low активирует BUY-ордера
	for price := 500.0; price >= 440.0; price -= 5 {
		e.AddCandle(Candle{Open: price, High: price + 3, Low: price - 8, Close: price})
	}

	btcAfterBuy := e.btcBal
	usdtAfterBuy := e.usdtBal

	// Equity в нижней точке
	equityLow := usdtAfterBuy + btcAfterBuy*440.0
	if equityLow <= 0 {
		t.Fatalf("equity после покупок ушёл в минус: %.2f", equityLow)
	}

	// Фаза 2: цена резко растёт — активируем SELL-ордера
	for price := 440.0; price <= 600.0; price += 5 {
		e.AddCandle(Candle{Open: price, High: price + 8, Low: price - 3, Close: price})
	}

	lastPrice := 600.0
	equityHigh := e.usdtBal + e.btcBal*lastPrice

	if equityHigh <= 0 {
		t.Fatalf("equity после продаж ушёл в минус: %.2f", equityHigh)
	}

	// После полного цикла down→up equity не должен быть ниже начального минус комиссии
	// (допуск 20% потерь на комиссии и неблагоприятное исполнение)
	minExpected := cfg.InitialUSDT * 0.80
	if equityHigh < minExpected {
		t.Errorf("equity=%.2f ниже допустимого минимума=%.2f", equityHigh, minExpected)
	}
}
