package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ─── CONFIG ──────────────────────────────────────────────────────────────────

const (
	DefaultSymbol = "BTCUSDT"
	CandleLimit   = 150
	WsURL         = "wss://stream.bybit.com/v5/public/spot"
	RestURL       = "https://api.bybit.com/v5/market/kline"
)

// ─── DATA TYPES ──────────────────────────────────────────────────────────────

type Candle struct {
	TimestampMs int64
	Open        float64
	High        float64
	Low         float64
	Close       float64
}

type Config struct {
	ATRPeriod     int
	ATRMultiplier float64
	SMAPeriod     int
	GridCount     int
	QuoteQty      float64
	FeeRate       float64
	InitialUSDT   float64
	TrailingStep  float64
}

type Order struct {
	Price  float64
	Side   int // 1: Buy, -1: Sell
	Active bool
}

// ─── ALERTS ──────────────────────────────────────────────────────────────────

// AlertKind описывает причину, по которой ордер не был исполнен.
type AlertKind string

const (
	// AlertInsufficientUSDT — BUY пропущен: баланс USDT меньше QuoteQty.
	AlertInsufficientUSDT AlertKind = "insufficient_usdt"
	// AlertInsufficientBTC — SELL пропущен: баланс BTC меньше стандартного лота.
	// Предотвращает создание зеркального BUY на полный QuoteQty после частичной продажи (overtrade).
	AlertInsufficientBTC AlertKind = "insufficient_btc"
)

// Alert несёт диагностическую информацию о пропущенном ордере.
type Alert struct {
	Kind     AlertKind
	LevelIdx int
	Price    float64
	Have     float64 // фактический баланс в момент проверки
	Need     float64 // требуемый минимум для исполнения
}

// ─── ENGINE ──────────────────────────────────────────────────────────────────

type Engine struct {
	cfg        Config
	levels     []float64
	orders     map[int]*Order
	usdtBal    float64
	btcBal     float64
	trades     int
	lastCenter float64
	inMarket   bool
	mu         sync.Mutex
	candles    []Candle

	// OnAlert вызывается при каждом пропущенном ордере. Опционален: nil — тихий режим.
	OnAlert func(Alert)
}

func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:     cfg,
		usdtBal: cfg.InitialUSDT,
		orders:  make(map[int]*Order),
		candles: make([]Candle, 0, CandleLimit),
	}
}

// emit вызывает OnAlert, если он установлен.
func (e *Engine) emit(a Alert) {
	if e.OnAlert != nil {
		e.OnAlert(a)
	}
}

func (e *Engine) calculateSMA() float64 {
	n := len(e.candles)
	if n < e.cfg.SMAPeriod {
		return 0
	}
	sum := 0.0
	for _, c := range e.candles[n-e.cfg.SMAPeriod:] {
		sum += c.Close
	}
	return sum / float64(e.cfg.SMAPeriod)
}

func (e *Engine) calculateATR() float64 {
	n := len(e.candles)
	if n <= e.cfg.ATRPeriod {
		return 0
	}
	start := n - e.cfg.ATRPeriod
	sum := 0.0
	for i := start; i < n; i++ {
		tr := math.Max(e.candles[i].High-e.candles[i].Low,
			math.Max(math.Abs(e.candles[i].High-e.candles[i-1].Close),
				math.Abs(e.candles[i].Low-e.candles[i-1].Close)))
		sum += tr
	}
	return sum / float64(e.cfg.ATRPeriod)
}

func (e *Engine) exitToCash(price float64) {
	if e.btcBal > 0 {
		e.usdtBal += e.btcBal * price * (1 - e.cfg.FeeRate)
		e.btcBal = 0
		e.trades++
	}
	e.orders = make(map[int]*Order)
	e.lastCenter = 0
	e.inMarket = false
	sma := e.calculateSMA()
	fmt.Printf("📉 ТРЕНД: Медвежий. Текущая цена: %.2f, SMA: %.2f. Виртуально выхожу в кэш. Баланс: %.2f USDT\n",
		price, sma, e.usdtBal)
}

func (e *Engine) setupGrid(price, atr float64) {
	e.inMarket = true
	dynamicRange := (atr * e.cfg.ATRMultiplier) / price
	if dynamicRange < 0.03 {
		dynamicRange = 0.03
	}

	halfRange := (price * dynamicRange) / 2
	step := (price * dynamicRange) / float64(e.cfg.GridCount)

	e.lastCenter = price
	e.orders = make(map[int]*Order)
	e.levels = make([]float64, e.cfg.GridCount+1)

	for i := 0; i <= e.cfg.GridCount; i++ {
		lvl := (price - halfRange) + float64(i)*step
		e.levels[i] = lvl
		if lvl < price {
			e.orders[i] = &Order{Price: lvl, Side: 1, Active: true}
		} else if lvl > price && e.btcBal > 0 {
			e.orders[i] = &Order{Price: lvl, Side: -1, Active: true}
		}
	}

	sma := e.calculateSMA()
	fmt.Printf("📈 ТРЕНД: Бычий. Текущая цена: %.2f, SMA: %.2f. Виртуально выставляю сетку уровней...\n",
		price, sma)
	for i, lvl := range e.levels {
		if ord, ok := e.orders[i]; ok {
			side := "BUY "
			if ord.Side == -1 {
				side = "SELL"
			}
			fmt.Printf("   [%d] %.2f → %s\n", i, lvl, side)
		}
	}
}

func (e *Engine) executeOrder(idx int, price float64) {
	ord, ok := e.orders[idx]
	if !ok || !ord.Active {
		return
	}

	action := ""
	if ord.Side == 1 { // BUY
		if e.usdtBal < e.cfg.QuoteQty {
			e.emit(Alert{Kind: AlertInsufficientUSDT, LevelIdx: idx, Price: price,
				Have: e.usdtBal, Need: e.cfg.QuoteQty})
			return
		}
		btcToBuy := (e.cfg.QuoteQty / price) * (1 - e.cfg.FeeRate)
		e.usdtBal -= e.cfg.QuoteQty
		e.btcBal += btcToBuy
		action = fmt.Sprintf("Куплено %.6f BTC", btcToBuy)
	} else { // SELL — симметричный guard: отклоняем, если BTC меньше полного лота
		qtyToSell := e.cfg.QuoteQty / price
		if e.btcBal < qtyToSell {
			e.emit(Alert{Kind: AlertInsufficientBTC, LevelIdx: idx, Price: price,
				Have: e.btcBal, Need: qtyToSell})
			return
		}
		e.usdtBal += qtyToSell * price * (1 - e.cfg.FeeRate)
		e.btcBal -= qtyToSell
		action = fmt.Sprintf("Продано %.6f BTC", qtyToSell)
	}

	e.trades++
	ord.Active = false

	// Переворачиваем ордер на соседний уровень
	mirrorIdx := idx - ord.Side
	if mirrorIdx >= 0 && mirrorIdx < len(e.levels) {
		e.orders[mirrorIdx] = &Order{Price: e.levels[mirrorIdx], Side: -ord.Side, Active: true}
	}

	equity := e.usdtBal + e.btcBal*price
	fmt.Printf("🔔 СРАБОТАЛ УРОВЕНЬ: %s на уровне %.2f. Виртуальный баланс: %.2f USDT (BTC: %.6f)\n",
		action, price, equity, e.btcBal)
}

// AddCandle — основной обработчик закрытой свечи.
// Логика входа (Price > SMA) и расчёты SMA/ATR не изменены.
func (e *Engine) AddCandle(c Candle) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.candles) >= CandleLimit {
		e.candles = e.candles[1:]
	}
	e.candles = append(e.candles, c)

	if len(e.candles) < e.cfg.SMAPeriod+1 {
		return
	}

	sma := e.calculateSMA()
	atr := e.calculateATR()

	// ── SMA Filter (не менять!) ──────────────────────────────────────────────
	if c.Close < sma {
		if e.inMarket {
			e.exitToCash(c.Close)
		}
		return
	}

	// ── Trailing ─────────────────────────────────────────────────────────────
	if e.lastCenter == 0 || math.Abs(c.Close-e.lastCenter)/e.lastCenter > e.cfg.TrailingStep {
		e.setupGrid(c.Close, atr)
	}

	// ── Scan grid ────────────────────────────────────────────────────────────
	for j := 0; j < len(e.levels); j++ {
		if lvl := e.levels[j]; lvl >= c.Low && lvl <= c.High {
			e.executeOrder(j, lvl)
		}
	}
}

// ─── BYBIT REST ──────────────────────────────────────────────────────────────

type bybitKlineResp struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		List [][]string `json:"list"`
	} `json:"result"`
}

func fetchHistory(symbol string) ([]Candle, error) {
	url := fmt.Sprintf("%s?category=spot&symbol=%s&interval=1&limit=%d", RestURL, symbol, CandleLimit)
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var kr bybitKlineResp
	if err := json.Unmarshal(body, &kr); err != nil {
		return nil, err
	}
	if kr.RetCode != 0 {
		return nil, fmt.Errorf("bybit API: %s", kr.RetMsg)
	}

	// list — от новых к старым, разворачиваем
	list := kr.Result.List
	candles := make([]Candle, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		item := list[i]
		if len(item) < 5 {
			continue
		}
		ts, _ := strconv.ParseInt(item[0], 10, 64)
		o, _ := strconv.ParseFloat(item[1], 64)
		h, _ := strconv.ParseFloat(item[2], 64)
		l, _ := strconv.ParseFloat(item[3], 64)
		c, _ := strconv.ParseFloat(item[4], 64)
		candles = append(candles, Candle{TimestampMs: ts, Open: o, High: h, Low: l, Close: c})
	}
	return candles, nil
}

// ─── BYBIT WEBSOCKET ─────────────────────────────────────────────────────────

type wsKlineMsg struct {
	Topic string `json:"topic"`
	Data  []struct {
		Start   int64  `json:"start"`
		Open    string `json:"open"`
		High    string `json:"high"`
		Low     string `json:"low"`
		Close   string `json:"close"`
		Confirm bool   `json:"confirm"`
	} `json:"data"`
}

func runWebSocket(symbol string, engine *Engine) {
	for {
		if err := connectWS(symbol, engine); err != nil {
			log.Printf("WebSocket ошибка: %v. Переподключение через 5с...", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func connectWS(symbol string, engine *Engine) error {
	conn, _, err := websocket.DefaultDialer.Dial(WsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	sub := fmt.Sprintf(`{"op":"subscribe","args":["kline.1.%s"]}`, symbol)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(sub)); err != nil {
		return err
	}
	log.Printf("✅ WebSocket подключён. Подписка: kline.1.%s", symbol)

	// Keepalive ping каждые 20 секунд
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"op":"ping"}`)); err != nil {
				return
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		var ws wsKlineMsg
		if err := json.Unmarshal(msg, &ws); err != nil {
			continue
		}
		if ws.Topic == "" || len(ws.Data) == 0 {
			continue
		}

		kline := ws.Data[0]
		// Обрабатываем только закрытые (подтверждённые) свечи
		if !kline.Confirm {
			continue
		}

		o, _ := strconv.ParseFloat(kline.Open, 64)
		h, _ := strconv.ParseFloat(kline.High, 64)
		l, _ := strconv.ParseFloat(kline.Low, 64)
		c, _ := strconv.ParseFloat(kline.Close, 64)

		t := time.UnixMilli(kline.Start).Format("15:04:05")
		log.Printf("[%s] Свеча закрыта: O=%.2f H=%.2f L=%.2f C=%.2f", t, o, h, l, c)

		engine.AddCandle(Candle{TimestampMs: kline.Start, Open: o, High: h, Low: l, Close: c})
	}
}

// ─── MAIN ────────────────────────────────────────────────────────────────────

func main() {
	symbol := DefaultSymbol
	if len(os.Args) > 1 {
		symbol = os.Args[1]
	}

	cfg := Config{
		ATRPeriod:     100,
		ATRMultiplier: 12.0,
		SMAPeriod:     100,
		GridCount:     8,
		InitialUSDT:   1000.0,
		QuoteQty:      300.0,
		FeeRate:       0.001,
		TrailingStep:  0.05,
	}

	engine := NewEngine(cfg)
	engine.OnAlert = func(a Alert) {
		log.Printf("⚠️  ALERT [%s] уровень=%d цена=%.2f имеется=%.6f нужно=%.6f",
			a.Kind, a.LevelIdx, a.Price, a.Have, a.Need)
	}

	fmt.Printf("🚀 Запуск Paper Trading | Символ: %s | Начальный баланс: %.0f USDT\n",
		symbol, cfg.InitialUSDT)

	fmt.Printf("📡 Загружаю историю (%d свечей) через REST API...\n", CandleLimit)
	history, err := fetchHistory(symbol)
	if err != nil {
		log.Fatalf("Ошибка загрузки истории: %v", err)
	}
	fmt.Printf("✅ Загружено %d свечей. Инициализирую движок...\n\n", len(history))

	for _, c := range history {
		engine.AddCandle(c)
	}

	go runWebSocket(symbol, engine)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	engine.mu.Lock()
	defer engine.mu.Unlock()
	equity := engine.usdtBal
	if len(engine.candles) > 0 {
		lastPrice := engine.candles[len(engine.candles)-1].Close
		equity += engine.btcBal * lastPrice
		fmt.Printf("\n🛑 Остановка. Итоговый баланс: %.2f USDT | Сделок: %d | Возврат: %.2f%%\n",
			equity, engine.trades, (equity/cfg.InitialUSDT-1)*100)
	}
}
