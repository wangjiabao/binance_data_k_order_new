package logic

import (
	"binance_data_gf/internal/model/entity"
	"binance_data_gf/internal/service"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gateio/gateapi-go/v6"
	"github.com/gogf/gf/v2/container/gmap"
	"github.com/gogf/gf/v2/container/gqueue"
	"github.com/gogf/gf/v2/os/grpool"
	"github.com/shopspring/decimal"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type (
	sBinanceTraderHistory struct {
		pool       *grpool.Pool
		ips        *gmap.IntStrMap
		orderQueue *gqueue.Queue
	}
)

func init() {
	service.RegisterBinanceTraderHistory(New())
}

func New() *sBinanceTraderHistory {
	return &sBinanceTraderHistory{
		grpool.New(), // 这里是请求协程池子，可以配合着可并行请求binance的限制使用，来限制最大共存数，后续jobs都将排队，考虑到上层的定时任务
		gmap.NewIntStrMap(true),
		gqueue.New(), // 下单顺序队列
	}
}

func IsEqual(f1, f2 float64) bool {
	if f1 > f2 {
		return f1-f2 < 0.000000001
	} else {
		return f2-f1 < 0.000000001
	}
}

func lessThanOrEqualZero(a, b float64, epsilon float64) bool {
	return a-b < epsilon || math.Abs(a-b) < epsilon
}

var (
	globalUsers = gmap.New(true)
	symbolsMap  = gmap.NewStrAnyMap(true)
	// 用户k线收盘价
	initPrice    = gmap.NewStrAnyMap(true)
	userOrderMap = gmap.New(true)
)

type SymbolGate struct {
	Symbol           string  `json:"symbol"            ` //
	QuantoMultiplier float64 `json:"quantityPrecision" ` //
	OrderPriceRound  int
}

func (s *sBinanceTraderHistory) DeleteUser(user *entity.NewUser) bool {
	if globalUsers.Contains(user.ApiKey) {
		tmpUser := globalUsers.Get(user.ApiKey).(*entity.NewUser)

		var (
			err error
		)
		tmpSq := symbolsMap.Get("BTCDOMUSDT").(*entity.LhCoinSymbol).QuantityPrecision
		tmpSqT := symbolsMap.Get("XRPUSDT").(*entity.LhCoinSymbol).QuantityPrecision

		tmpStrUserId := strconv.FormatUint(uint64(tmpUser.Id), 10)
		userDomKey := "BTCDOMUSDT" + tmpStrUserId
		userEthKey := "XRPUSDT" + tmpStrUserId

		// 平仓
		if userOrderMap.Contains(userEthKey) {
			// 开仓数量
			tmpQty := userOrderMap.Get(userEthKey).(float64)
			log.Println("删除信息，eth", tmpQty)

			// 精度调整
			var (
				quantity      string
				quantityFloat float64
			)
			if 0 >= tmpSqT {
				quantity = fmt.Sprintf("%d", int64(tmpQty))
			} else {
				quantity = strconv.FormatFloat(tmpQty, 'f', tmpSqT, 64)
			}

			quantityFloat, err = strconv.ParseFloat(quantity, 64)
			if nil != err {
				log.Println("删除，数量信息错误", err)
			}

			// binance
			var (
				binanceOrderRes *binanceOrder
				orderInfoRes    *orderInfo
				errA            error
			)

			if !lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
				// 请求下单
				binanceOrderRes, orderInfoRes, errA = requestBinanceOrder("XRPUSDT", "SELL", "MARKET", "LONG", quantity, tmpUser.ApiKey, tmpUser.ApiSecret)
				if nil != errA || binanceOrderRes.OrderId <= 0 {
					log.Println("删除，仓位，信息：", errA, binanceOrderRes, orderInfoRes, quantity)
				}
			}
		}

		// 平仓
		if userOrderMap.Contains(userDomKey) {
			// 开仓数量
			tmpQty := userOrderMap.Get(userDomKey).(float64)
			log.Println("删除信息，dom", tmpQty)

			// 精度调整
			var (
				quantity      string
				quantityFloat float64
			)
			if 0 >= tmpSq {
				quantity = fmt.Sprintf("%d", int64(tmpQty))
			} else {
				quantity = strconv.FormatFloat(tmpQty, 'f', tmpSq, 64)
			}

			quantityFloat, err = strconv.ParseFloat(quantity, 64)
			if nil != err {
				log.Println("删除，数量信息错误", err)
			}

			// binance
			var (
				binanceOrderRes *binanceOrder
				orderInfoRes    *orderInfo
				errA            error
			)

			if !lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
				// 请求下单
				binanceOrderRes, orderInfoRes, errA = requestBinanceOrder("BTCDOMUSDT", "SELL", "MARKET", "LONG", quantity, tmpUser.ApiKey, tmpUser.ApiSecret)
				if nil != errA || binanceOrderRes.OrderId <= 0 {
					log.Println("删除，仓位，信息：", errA, binanceOrderRes, orderInfoRes, quantity)
				}
			}
		}

		userOrderMap.Set(userEthKey, float64(0))
		userOrderMap.Set(userDomKey, float64(0))
		initPrice.Set(userDomKey, float64(0))
		initPrice.Set("XRPBTC"+tmpStrUserId, float64(0))
		globalUsers.Remove(user.ApiKey)
	}

	return true
}

func (s *sBinanceTraderHistory) UpdateUser(user *entity.NewUser) bool {
	if globalUsers.Contains(user.ApiKey) {
		tmpUser := globalUsers.Get(user.ApiKey).(*entity.NewUser)
		globalUsers.Set(user.ApiKey, &entity.NewUser{
			Id:        tmpUser.Id,
			ApiKey:    tmpUser.ApiKey,
			ApiSecret: tmpUser.ApiSecret,
			Num:       user.Num,
			First:     user.First,
			Second:    user.Second,
		})
	}

	return true
}

func (s *sBinanceTraderHistory) GetUsers() []*entity.UserInfo {
	users := make([]*entity.UserInfo, 0)

	globalUsers.Iterator(func(k interface{}, v interface{}) bool {
		tmpUser := v.(*entity.NewUser)
		tmpStrUserId := strconv.FormatUint(uint64(tmpUser.Id), 10)
		userDomKey := "BTCDOMUSDT" + tmpStrUserId
		userEthKey := "XRPUSDT" + tmpStrUserId

		tmp1 := float64(0)
		if userOrderMap.Contains(userDomKey) {
			tmp1 = userOrderMap.Get(userDomKey).(float64)
		}

		tmp2 := float64(0)
		if userOrderMap.Contains(userEthKey) {
			tmp2 = userOrderMap.Get(userEthKey).(float64)
		}

		users = append(users, &entity.UserInfo{
			ApiKey:    tmpUser.ApiKey,
			ApiSecret: tmpUser.ApiSecret,
			Num:       tmpUser.Num,
			First:     tmpUser.First,
			Second:    tmpUser.Second,
			Eth:       tmp2,
			Dom:       tmp1,
		})

		return true
	})

	return users
}

// 锁，防止并发场景，id会重复
var insertLock sync.Mutex

// InsertUser 初始化信息
func (s *sBinanceTraderHistory) InsertUser(userP *entity.NewUser) bool {
	insertLock.Lock()
	defer insertLock.Unlock()

	if globalUsers.Contains(userP.ApiKey) {
		return false
	}

	maxId := uint(0)
	globalUsers.Iterator(func(k interface{}, v interface{}) bool {
		tmpUser := v.(*entity.NewUser)
		if maxId <= tmpUser.Id {
			maxId = tmpUser.Id
		}

		return true
	})

	user := &entity.NewUser{
		Id:        maxId + 1,
		ApiKey:    userP.ApiKey,
		ApiSecret: userP.ApiSecret,
		Num:       userP.Num,
		First:     userP.First,
		Second:    userP.Second,
	}
	globalUsers.Set(user.ApiKey, user)

	startMs, endMs := getLast15Area(0)
	if 0 >= len(startMs) || 0 >= len(endMs) {
		return false
	}

	var (
		err        error
		tmpDomName = "BTCDOMUSDT"
		tmpEthName = "XRPBTC"
	)

	var (
		kLines []*KLineU
	)
	kLines, err = requestBinanceFuturesKLines(tmpDomName, "15m", startMs, endMs, "1")
	if err != nil {
		log.Println(err, "查询k线错误")
		return false
	}

	// 打印结果（使用中国时间显示）
	for _, kv := range kLines {
		var (
			tmpCurrentPrice float64
		)
		tmpCurrentPrice, _ = strconv.ParseFloat(kv.Close, 10)
		if 0 >= tmpCurrentPrice {
			fmt.Println("价格0", kv)
			continue
		}

		tmpStrUserId := strconv.FormatUint(uint64(user.Id), 10)

		initPrice.Set(tmpDomName+tmpStrUserId, tmpCurrentPrice)
		log.Println("初始化合约：", user, tmpDomName, kv.Close, tmpCurrentPrice)
	}

	var (
		kLinesEthBTC []*KLineDay
	)
	kLinesEthBTC, err = requestBinanceDailyKLines(tmpEthName, "15m", startMs, endMs, "1")
	if err != nil {
		log.Println(err, "查询k线错误")
		return false
	}

	// 打印结果（使用中国时间显示）
	for _, kv := range kLinesEthBTC {
		var (
			tmpCurrentPrice float64
		)
		tmpCurrentPrice, _ = strconv.ParseFloat(kv.Close, 10)
		if 0 >= tmpCurrentPrice {
			fmt.Println("价格0", kv)
			continue
		}

		tmpStrUserId := strconv.FormatUint(uint64(user.Id), 10)
		initPrice.Set(tmpEthName+tmpStrUserId, tmpCurrentPrice)
		log.Println("初始化现货：", user, tmpEthName, kv.Close, tmpCurrentPrice)
	}

	if !symbolsMap.Contains("XRPUSDT") || !symbolsMap.Contains("BTCDOMUSDT") {
		log.Println("不存在币种信息")
		return false
	}

	tmpSq := symbolsMap.Get("BTCDOMUSDT").(*entity.LhCoinSymbol).QuantityPrecision
	tmpSqT := symbolsMap.Get("XRPUSDT").(*entity.LhCoinSymbol).QuantityPrecision

	// 下单 开空
	var (
		priceEth         float64
		priceDom         float64
		coinUsdtPriceEth *FuturesPrice
		coinUsdtPriceDom *FuturesPrice
	)
	coinUsdtPriceEth, err = getUSDMFuturesPrice("XRPUSDT")
	if nil != err {
		log.Println("价格查询错误，eth", err)
		return false
	}
	priceEth, err = strconv.ParseFloat(coinUsdtPriceEth.Price, 10)
	if 0 >= priceEth {
		fmt.Println("价格0，usdt，eth", user)
		return false
	}

	coinUsdtPriceDom, err = getUSDMFuturesPrice("BTCDOMUSDT")
	if nil != err {
		log.Println("价格查询错误，dom", err)
		return false
	}
	priceDom, err = strconv.ParseFloat(coinUsdtPriceDom.Price, 10)
	if 0 >= priceDom {
		fmt.Println("价格0，usdt，dom", user)
		return false
	}

	tmpStrUserId := strconv.FormatUint(uint64(user.Id), 10)
	userDomKey := "BTCDOMUSDT" + tmpStrUserId
	userEthKey := "XRPUSDT" + tmpStrUserId

	// 开eth

	// 开仓数量
	tmpQty := user.Num / priceEth
	log.Println("初始化信息，eth", tmpQty)

	// 精度调整
	var (
		quantity      string
		quantityFloat float64
	)
	if 0 >= tmpSqT {
		quantity = fmt.Sprintf("%d", int64(tmpQty))
	} else {
		quantity = strconv.FormatFloat(tmpQty, 'f', tmpSqT, 64)
	}

	quantityFloat, err = strconv.ParseFloat(quantity, 64)
	if nil != err {
		log.Println("开仓数量信息错误", err)
		return false
	}

	// binance
	var (
		binanceOrderRes *binanceOrder
		orderInfoRes    *orderInfo
		errA            error
	)

	if !lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
		// 请求下单
		binanceOrderRes, orderInfoRes, errA = requestBinanceOrder("XRPUSDT", "BUY", "MARKET", "LONG", quantity, user.ApiKey, user.ApiSecret)
		if nil != errA || binanceOrderRes.OrderId <= 0 {
			log.Println("仓位，信息：", errA, binanceOrderRes, orderInfoRes, quantity)
			return false
		} else {
			userOrderMap.Set(userEthKey, quantityFloat)
		}
	} else {
		log.Println("开仓数量太小", quantityFloat)
		return false
	}

	// 开dom

	// 开仓数量
	tmpQtyTwo := user.Num / priceDom
	log.Println("初始化信息，dom", tmpQtyTwo)

	// 精度调整
	var (
		quantityTwo      string
		quantityFloatTwo float64
	)
	if 0 >= tmpSq {
		quantityTwo = fmt.Sprintf("%d", int64(tmpQtyTwo))
	} else {
		quantityTwo = strconv.FormatFloat(tmpQtyTwo, 'f', tmpSq, 64)
	}

	quantityFloatTwo, err = strconv.ParseFloat(quantityTwo, 64)
	if nil != err {
		log.Println("开仓数量信息错误", err)
		return false
	}

	// binance
	var (
		binanceOrderResTwo *binanceOrder
		orderInfoResTwo    *orderInfo
		errB               error
	)

	if !lessThanOrEqualZero(quantityFloatTwo, 0, 1e-7) {
		// 请求下单
		binanceOrderResTwo, orderInfoResTwo, errB = requestBinanceOrder("BTCDOMUSDT", "BUY", "MARKET", "LONG", quantityTwo, user.ApiKey, user.ApiSecret)
		if nil != errB || binanceOrderResTwo.OrderId <= 0 {
			log.Println("仓位，信息：", errB, binanceOrderResTwo, orderInfoResTwo, quantityTwo)
			return false
		} else {
			userOrderMap.Set(userDomKey, quantityFloatTwo)
		}
	} else {
		log.Println("开仓数量太小", quantityFloatTwo)
		return false
	}

	return true
}

// UpdateCoinInfo 初始化信息
func (s *sBinanceTraderHistory) UpdateCoinInfo(ctx context.Context) bool {
	//// 获取代币信息
	//var (
	//	err     error
	//	symbols []*entity.LhCoinSymbol
	//)
	//err = g.Model("lh_coin_symbol").Ctx(ctx).Scan(&symbols)
	//if nil != err || 0 >= len(symbols) {
	//	fmt.Println("龟兔，初始化，币种，数据库查询错误：", err)
	//	return false
	//}
	//// 处理
	//for _, vSymbols := range symbols {
	//	symbolsMap.Set(vSymbols.Symbol+"USDT", vSymbols)
	//}
	//
	//return true

	// 获取代币信息
	var (
		err               error
		binanceSymbolInfo []*BinanceSymbolInfo
	)
	binanceSymbolInfo, err = getBinanceFuturesPairs()
	if nil != err {
		log.Println("更新币种，binance", err)
		return false
	}

	for _, v := range binanceSymbolInfo {
		symbolsMap.Set(v.Symbol, &entity.LhCoinSymbol{
			Id:                0,
			Coin:              v.BaseAsset,
			Symbol:            v.Symbol,
			StartTime:         0,
			EndTime:           0,
			PricePrecision:    v.PricePrecision,
			QuantityPrecision: v.QuantityPrecision,
			IsOpen:            0,
		})
	}

	//var (
	//	resGate []gateapi.Contract
	//)
	//
	//resGate, err = getGateContract()
	//if nil != err {
	//	log.Println("更新币种， gate", err)
	//	return false
	//}
	//
	//for _, v := range resGate {
	//	var (
	//		tmp  float64
	//		tmp2 int
	//	)
	//	tmp, err = strconv.ParseFloat(v.QuantoMultiplier, 64)
	//	if nil != err {
	//		continue
	//	}
	//
	//	tmp2 = getDecimalPlaces(v.OrderPriceRound)
	//
	//	base := strings.TrimSuffix(v.Name, "_USDT")
	//	symbolsMapGate.Set(base+"USDT", &SymbolGate{
	//		Symbol:           v.Name,
	//		QuantoMultiplier: tmp,
	//		OrderPriceRound:  tmp2,
	//	})
	//}

	return true
}

func floatGreater(a, b, epsilon float64) bool {
	return a-b >= epsilon
}

func getLast15Area(slot uint64) (string, string) {
	var (
		loc *time.Location
		err error
	)
	loc, err = time.LoadLocation("Asia/Shanghai")
	if err != nil {
		log.Println("无法加载 Asia/Shanghai 时区: " + err.Error())
		return "", ""
	}

	if slot == 0 {
		slot = 1
	}

	now := time.Now().In(loc)

	// 对齐到当前时间的上一个 15 分钟整点
	minute := now.Minute()
	alignedMinute := (minute / 15) * 15
	currentSlotEnd := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), alignedMinute, 0, 0, loc)

	// 计算目标 slot 的结束时间，减去1ms
	slotDuration := 15 * time.Minute
	endTime := currentSlotEnd.Add(-time.Duration(slot-1) * slotDuration).Add(-time.Millisecond)
	startTime := endTime.Add(-slotDuration + time.Millisecond) // 保持时间段长度为15分钟

	return fmt.Sprintf("%d", startTime.UnixMilli()), fmt.Sprintf("%d", endTime.UnixMilli())

}

// HandleKLineNew .
func (s *sBinanceTraderHistory) HandleKLineNew(ctx context.Context) {
	startMs, endMs := getLast15Area(0)
	if 0 >= len(startMs) || 0 >= len(endMs) {
		return
	}

	var (
		err        error
		domName    = "BTCDOMUSDT"
		ethName    = "XRPUSDT"
		ethBTCName = "XRPBTC"
	)

	if !symbolsMap.Contains(domName) || !symbolsMap.Contains(ethName) {
		log.Println("不存在币种信息")
		return
	}

	tmpSq := symbolsMap.Get(domName).(*entity.LhCoinSymbol).QuantityPrecision
	tmpSqT := symbolsMap.Get(ethName).(*entity.LhCoinSymbol).QuantityPrecision

	var (
		kLines []*KLineU
	)
	kLines, err = requestBinanceFuturesKLines(domName, "15m", startMs, endMs, "1")
	if err != nil {
		log.Println(err, "查询k线错误")
		return
	}

	// 打印结果（使用中国时间显示）
	for _, k := range kLines {
		var (
			tmpCurrentPrice float64
		)
		tmpCurrentPrice, _ = strconv.ParseFloat(k.Close, 10)
		if 0 >= tmpCurrentPrice {
			fmt.Println("价格0", k)
			continue
		}

		// 下单 开空
		var (
			priceEth         float64
			priceDom         float64
			coinUsdtPriceEth *FuturesPrice
			coinUsdtPriceDom *FuturesPrice
		)
		coinUsdtPriceEth, err = getUSDMFuturesPrice(ethName)
		if nil != err {
			log.Println("价格查询错误，eth", err)
			continue
		}
		priceEth, err = strconv.ParseFloat(coinUsdtPriceEth.Price, 10)
		if 0 >= priceEth {
			fmt.Println("价格0，usdt，eth", k)
			continue
		}

		coinUsdtPriceDom, err = getUSDMFuturesPrice(domName)
		if nil != err {
			log.Println("价格查询错误，dom", err)
			continue
		}
		priceDom, err = strconv.ParseFloat(coinUsdtPriceDom.Price, 10)
		if 0 >= priceDom {
			fmt.Println("价格0，usdt，dom", k)
			continue
		}

		globalUsers.Iterator(func(ku interface{}, v interface{}) bool {
			tmpUser := v.(*entity.NewUser)
			tmpStrUserId := strconv.FormatUint(uint64(tmpUser.Id), 10)
			userDomKey := domName + tmpStrUserId
			userEthKey := ethName + tmpStrUserId

			// 初始化
			if !initPrice.Contains(userDomKey) {
				initPrice.Set(userDomKey, tmpCurrentPrice)
				log.Println("合约：", domName, k.Close, tmpCurrentPrice)
				return true
			}

			tmpInitPrice := initPrice.Get(userDomKey).(float64)

			if floatGreater(tmpCurrentPrice, tmpInitPrice, 1e-8) {
				// 涨价
				tmpSubRate := (tmpCurrentPrice - tmpInitPrice) / tmpInitPrice
				if !floatGreater(tmpSubRate, tmpUser.First, 1e-4) {
					return true
				}

				// 更新初始化价格
				initPrice.Set(userDomKey, tmpCurrentPrice)

				if !userOrderMap.Contains(userDomKey) {
					log.Println("错误，无仓位", userDomKey)
					return true
				}

				domQty := userOrderMap.Get(userDomKey).(float64)
				ethQty := userOrderMap.Get(userEthKey).(float64)
				if 0 >= domQty || 0 >= ethQty {
					log.Println("无仓位", userDomKey, userEthKey, domQty, ethQty)
					return true
				}

				qtyRate := float64(1)
				// dom重
				if floatGreater(domQty*priceDom, ethQty*priceEth, 1e-4) {
					qtyRate = ethQty * priceEth / (domQty * priceDom)
				} else if floatGreater(ethQty*priceEth, domQty*priceDom, 1e-4) {
					qtyRate = domQty * priceDom / (ethQty * priceEth)
				}

				// 关仓数量
				tmpQty := qtyRate * tmpSubRate * tmpUser.Second / priceDom
				log.Println("关仓信息，dom", tmpInitPrice, tmpCurrentPrice, tmpUser, domQty, ethQty, priceDom, priceEth, qtyRate, tmpQty)

				// 精度调整
				var (
					quantity      string
					quantityFloat float64
				)
				if 0 >= tmpSq {
					quantity = fmt.Sprintf("%d", int64(tmpQty))
				} else {
					quantity = strconv.FormatFloat(tmpQty, 'f', tmpSq, 64)
				}

				quantityFloat, err = strconv.ParseFloat(quantity, 64)
				if nil != err {
					log.Println("关仓数量信息错误", err)
					return true
				}

				if !userOrderMap.Contains(userDomKey) {
					log.Println("关仓数量信息错误，无仓位系统", err)
					return true
				}

				if lessThanOrEqualZero(userOrderMap.Get(userDomKey).(float64), quantityFloat, 1e-7) {
					log.Println("关仓数量信息错误，无仓位系统", userDomKey, userOrderMap.Get(userDomKey).(float64), quantityFloat)
					return true
				}

				// binance
				var (
					binanceOrderRes *binanceOrder
					orderInfoRes    *orderInfo
					errA            error
				)

				if !lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
					// 请求下单
					binanceOrderRes, orderInfoRes, errA = requestBinanceOrder(domName, "SELL", "MARKET", "LONG", quantity, tmpUser.ApiKey, tmpUser.ApiSecret)
					if nil != errA || binanceOrderRes.OrderId <= 0 {
						log.Println("仓位，信息：", errA, binanceOrderRes, orderInfoRes, quantity)
					} else {
						if userOrderMap.Contains(userDomKey) {
							d1 := decimal.NewFromFloat(userOrderMap.Get(userDomKey).(float64))
							d2 := decimal.NewFromFloat(quantityFloat)
							result := d1.Sub(d2)

							var (
								newRes float64
								exact  bool
							)
							newRes, exact = result.Float64()
							if !exact {
								fmt.Println("转换过程中可能发生了精度损失", d1, d2, quantityFloat, userOrderMap.Get(userDomKey).(float64), newRes)
							}

							if lessThanOrEqualZero(newRes, 0, 1e-7) {
								newRes = 0
							}

							userOrderMap.Set(userDomKey, newRes)
						}
					}

					time.Sleep(100 * time.Millisecond)
				} else {
					log.Println("关仓数量太小", quantityFloat)
				}

			} else if floatGreater(tmpInitPrice, tmpCurrentPrice, 1e-8) {
				// 跌价
				tmpSubRate := (tmpInitPrice - tmpCurrentPrice) / tmpInitPrice
				// First字段表示程度0.1，0.01等等
				if !floatGreater(tmpSubRate, tmpUser.First, 1e-4) {
					return true
				}

				// 更新初始化价格
				initPrice.Set(userDomKey, tmpCurrentPrice)

				if !userOrderMap.Contains(userDomKey) {
					log.Println("错误，无仓位", userDomKey)
					return true
				}

				domQty := userOrderMap.Get(userDomKey).(float64)
				ethQty := userOrderMap.Get(userEthKey).(float64)
				if 0 >= domQty || 0 >= ethQty {
					log.Println("无仓位", userDomKey, userEthKey, domQty, ethQty)
					return true
				}

				qtyRate := float64(1)
				// dom重
				if floatGreater(domQty*priceDom, ethQty*priceEth, 1e-4) {
					qtyRate = ethQty * priceEth / (domQty * priceDom)
				} else if floatGreater(ethQty*priceEth, domQty*priceDom, 1e-4) {
					qtyRate = domQty * priceDom / (ethQty * priceEth)
				}

				// 开仓数量
				tmpQty := qtyRate * tmpSubRate * tmpUser.Second / priceDom
				log.Println("开仓信息，dom", tmpInitPrice, tmpCurrentPrice, tmpUser, domQty, ethQty, priceDom, priceEth, qtyRate, tmpQty)

				// 精度调整
				var (
					quantity      string
					quantityFloat float64
				)
				if 0 >= tmpSq {
					quantity = fmt.Sprintf("%d", int64(tmpQty))
				} else {
					quantity = strconv.FormatFloat(tmpQty, 'f', tmpSq, 64)
				}

				quantityFloat, err = strconv.ParseFloat(quantity, 64)
				if nil != err {
					log.Println("开仓数量信息错误", err)
					return true
				}

				// binance
				var (
					binanceOrderRes *binanceOrder
					orderInfoRes    *orderInfo
					errA            error
				)

				if !lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
					// 请求下单
					binanceOrderRes, orderInfoRes, errA = requestBinanceOrder(domName, "BUY", "MARKET", "LONG", quantity, tmpUser.ApiKey, tmpUser.ApiSecret)
					if nil != errA || binanceOrderRes.OrderId <= 0 {
						log.Println("仓位，信息：", errA, binanceOrderRes, orderInfoRes, quantity)
					} else {
						if userOrderMap.Contains(userDomKey) {
							d1 := decimal.NewFromFloat(userOrderMap.Get(userDomKey).(float64))
							d2 := decimal.NewFromFloat(quantityFloat)
							result := d1.Add(d2)

							var (
								newRes float64
								exact  bool
							)
							newRes, exact = result.Float64()
							if !exact {
								fmt.Println("转换过程中可能发生了精度损失", d1, d2, quantityFloat, userOrderMap.Get(userDomKey).(float64), newRes)
							}

							userOrderMap.Set(userDomKey, newRes)
						} else {
							userOrderMap.Set(userDomKey, quantityFloat)
						}
					}

					time.Sleep(100 * time.Millisecond)
				} else {
					log.Println("开仓数量太小", quantityFloat)
				}

			} else {
				fmt.Println("价格没变", v, tmpCurrentPrice, tmpInitPrice)
				return true
			}

			return true
		})
	}

	var (
		kLinesETHBTC []*KLineDay
	)
	kLinesETHBTC, err = requestBinanceDailyKLines(ethBTCName, "15m", startMs, endMs, "1")
	if err != nil {
		log.Println(err, "查询k线错误")
		return
	}

	// 打印结果（使用中国时间显示）
	for _, k := range kLinesETHBTC {
		var (
			tmpCurrentPrice float64
		)
		tmpCurrentPrice, _ = strconv.ParseFloat(k.Close, 10)
		if 0 >= tmpCurrentPrice {
			fmt.Println("价格0", k)
			continue
		}

		// 下单 开空
		var (
			priceEth         float64
			priceDom         float64
			coinUsdtPriceEth *FuturesPrice
			coinUsdtPriceDom *FuturesPrice
		)
		coinUsdtPriceEth, err = getUSDMFuturesPrice(ethName)
		if nil != err {
			log.Println("价格查询错误，eth", err)
			continue
		}
		priceEth, err = strconv.ParseFloat(coinUsdtPriceEth.Price, 10)
		if 0 >= priceEth {
			fmt.Println("价格0，usdt，eth", k)
			continue
		}

		coinUsdtPriceDom, err = getUSDMFuturesPrice(domName)
		if nil != err {
			log.Println("价格查询错误，dom", err)
			continue
		}
		priceDom, err = strconv.ParseFloat(coinUsdtPriceDom.Price, 10)
		if 0 >= priceDom {
			fmt.Println("价格0，usdt，dom", k)
			continue
		}

		globalUsers.Iterator(func(ku interface{}, v interface{}) bool {
			tmpUser := v.(*entity.NewUser)
			tmpStrUserId := strconv.FormatUint(uint64(tmpUser.Id), 10)
			userDomKey := domName + tmpStrUserId
			userEthKey := ethName + tmpStrUserId

			// 初始化
			if !initPrice.Contains(userEthKey) {
				initPrice.Set(userEthKey, tmpCurrentPrice)
				log.Println("合约：", domName, k.Close, tmpCurrentPrice)
				return true
			}

			tmpInitPrice := initPrice.Get(ethBTCName).(float64)

			if floatGreater(tmpCurrentPrice, tmpInitPrice, 1e-8) {
				// 涨价
				tmpSubRate := (tmpCurrentPrice - tmpInitPrice) / tmpInitPrice
				if !floatGreater(tmpSubRate, tmpUser.First, 1e-4) {
					return true
				}

				// 更新初始化价格
				initPrice.Set(ethBTCName, tmpCurrentPrice)

				if !userOrderMap.Contains(userEthKey) {
					log.Println("错误，无仓位", userEthKey)
					return true
				}

				domQty := userOrderMap.Get(userDomKey).(float64)
				ethQty := userOrderMap.Get(userEthKey).(float64)
				if 0 >= domQty || 0 >= ethQty {
					log.Println("无仓位", userDomKey, userEthKey, domQty, ethQty)
					return true
				}

				qtyRate := float64(1)
				// dom重
				if floatGreater(domQty*priceDom, ethQty*priceEth, 1e-4) {
					qtyRate = ethQty * priceEth / (domQty * priceDom)
				} else if floatGreater(ethQty*priceEth, domQty*priceDom, 1e-4) {
					qtyRate = domQty * priceDom / (ethQty * priceEth)
				}

				// 关仓数量
				tmpQty := qtyRate * tmpSubRate * tmpUser.Second / priceEth
				log.Println("关仓信息，eth", tmpInitPrice, tmpCurrentPrice, tmpUser, domQty, ethQty, priceDom, priceEth, qtyRate, tmpQty)

				// 精度调整
				var (
					quantity      string
					quantityFloat float64
				)
				if 0 >= tmpSqT {
					quantity = fmt.Sprintf("%d", int64(tmpQty))
				} else {
					quantity = strconv.FormatFloat(tmpQty, 'f', tmpSqT, 64)
				}

				quantityFloat, err = strconv.ParseFloat(quantity, 64)
				if nil != err {
					log.Println("关仓数量信息错误", err)
					return true
				}

				if !userOrderMap.Contains(userEthKey) {
					log.Println("关仓数量信息错误，无仓位系统", err)
					return true
				}

				if lessThanOrEqualZero(userOrderMap.Get(userEthKey).(float64), quantityFloat, 1e-7) {
					log.Println("关仓数量信息错误，无仓位系统", userEthKey, userOrderMap.Get(userEthKey).(float64), quantityFloat)
					return true
				}

				// binance
				var (
					binanceOrderRes *binanceOrder
					orderInfoRes    *orderInfo
					errA            error
				)

				if !lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
					// 请求下单
					binanceOrderRes, orderInfoRes, errA = requestBinanceOrder(ethName, "SELL", "MARKET", "LONG", quantity, tmpUser.ApiKey, tmpUser.ApiSecret)
					if nil != errA || binanceOrderRes.OrderId <= 0 {
						log.Println("仓位，信息：", errA, binanceOrderRes, orderInfoRes, quantity)
					} else {
						if userOrderMap.Contains(userEthKey) {
							d1 := decimal.NewFromFloat(userOrderMap.Get(userEthKey).(float64))
							d2 := decimal.NewFromFloat(quantityFloat)
							result := d1.Sub(d2)

							var (
								newRes float64
								exact  bool
							)
							newRes, exact = result.Float64()
							if !exact {
								fmt.Println("转换过程中可能发生了精度损失", d1, d2, quantityFloat, userOrderMap.Get(userEthKey).(float64), newRes)
							}

							if lessThanOrEqualZero(newRes, 0, 1e-7) {
								newRes = 0
							}

							userOrderMap.Set(userEthKey, newRes)
						}
					}

					time.Sleep(100 * time.Millisecond)
				} else {
					log.Println("关仓数量太小", quantityFloat)
				}

			} else if floatGreater(tmpInitPrice, tmpCurrentPrice, 1e-8) {
				// 跌价
				tmpSubRate := (tmpInitPrice - tmpCurrentPrice) / tmpInitPrice
				// First字段表示程度0.1，0.01等等
				if !floatGreater(tmpSubRate, tmpUser.First, 1e-4) {
					return true
				}

				// 更新初始化价格
				initPrice.Set(ethBTCName, tmpCurrentPrice)

				if !userOrderMap.Contains(userEthKey) {
					log.Println("错误，无仓位", userEthKey)
					return true
				}

				domQty := userOrderMap.Get(userDomKey).(float64)
				ethQty := userOrderMap.Get(userEthKey).(float64)
				if 0 >= domQty || 0 >= ethQty {
					log.Println("无仓位", userDomKey, userEthKey, domQty, ethQty)
					return true
				}

				qtyRate := float64(1)
				// dom重
				if floatGreater(domQty*priceDom, ethQty*priceEth, 1e-4) {
					qtyRate = ethQty * priceEth / (domQty * priceDom)
				} else if floatGreater(ethQty*priceEth, domQty*priceDom, 1e-4) {
					qtyRate = domQty * priceDom / (ethQty * priceEth)
				}

				// 开仓数量
				tmpQty := qtyRate * tmpSubRate * tmpUser.Second / priceEth
				log.Println("开仓信息，eth", tmpInitPrice, tmpCurrentPrice, tmpUser, domQty, ethQty, priceDom, priceEth, qtyRate, tmpQty)

				// 精度调整
				var (
					quantity      string
					quantityFloat float64
				)
				if 0 >= tmpSqT {
					quantity = fmt.Sprintf("%d", int64(tmpQty))
				} else {
					quantity = strconv.FormatFloat(tmpQty, 'f', tmpSqT, 64)
				}

				quantityFloat, err = strconv.ParseFloat(quantity, 64)
				if nil != err {
					log.Println("开仓数量信息错误", err)
					return true
				}

				// binance
				var (
					binanceOrderRes *binanceOrder
					orderInfoRes    *orderInfo
					errA            error
				)

				if !lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
					// 请求下单
					binanceOrderRes, orderInfoRes, errA = requestBinanceOrder(ethName, "BUY", "MARKET", "LONG", quantity, tmpUser.ApiKey, tmpUser.ApiSecret)
					if nil != errA || binanceOrderRes.OrderId <= 0 {
						log.Println("仓位，信息：", errA, binanceOrderRes, orderInfoRes, quantity)
					} else {
						if userOrderMap.Contains(userEthKey) {
							d1 := decimal.NewFromFloat(userOrderMap.Get(userEthKey).(float64))
							d2 := decimal.NewFromFloat(quantityFloat)
							result := d1.Add(d2)

							var (
								newRes float64
								exact  bool
							)
							newRes, exact = result.Float64()
							if !exact {
								fmt.Println("转换过程中可能发生了精度损失", d1, d2, quantityFloat, userOrderMap.Get(userEthKey).(float64), newRes)
							}

							userOrderMap.Set(userEthKey, newRes)
						} else {
							userOrderMap.Set(userEthKey, quantityFloat)
						}
					}

					time.Sleep(100 * time.Millisecond)
				} else {
					log.Println("开仓数量太小", quantityFloat)
				}

			} else {
				fmt.Println("价格没变", v, tmpCurrentPrice, tmpInitPrice)
				return true
			}

			return true
		})
	}
}

// BinancePosition 代表单个头寸（持仓）信息
type BinancePosition struct {
	Symbol                 string `json:"symbol"`                 // 交易对
	InitialMargin          string `json:"initialMargin"`          // 当前所需起始保证金(基于最新标记价格)
	MaintMargin            string `json:"maintMargin"`            // 维持保证金
	UnrealizedProfit       string `json:"unrealizedProfit"`       // 持仓未实现盈亏
	PositionInitialMargin  string `json:"positionInitialMargin"`  // 持仓所需起始保证金(基于最新标记价格)
	OpenOrderInitialMargin string `json:"openOrderInitialMargin"` // 当前挂单所需起始保证金(基于最新标记价格)
	Leverage               string `json:"leverage"`               // 杠杆倍率
	Isolated               bool   `json:"isolated"`               // 是否是逐仓模式
	EntryPrice             string `json:"entryPrice"`             // 持仓成本价
	MaxNotional            string `json:"maxNotional"`            // 当前杠杆下用户可用的最大名义价值
	BidNotional            string `json:"bidNotional"`            // 买单净值，忽略
	AskNotional            string `json:"askNotional"`            // 卖单净值，忽略
	PositionSide           string `json:"positionSide"`           // 持仓方向 (BOTH, LONG, SHORT)
	PositionAmt            string `json:"positionAmt"`            // 持仓数量
	UpdateTime             int64  `json:"updateTime"`             // 更新时间
}

// floatEqual 判断两个浮点数是否在精度范围内相等
func floatEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) <= epsilon
}

// 获取币安服务器时间
func getBinanceServerTime() int64 {
	urlTmp := "https://api.binance.com/api/v3/time"
	resp, err := http.Get(urlTmp)
	if err != nil {
		log.Println("Error getting server time:", err)
		return 0
	}

	defer func() {
		if resp != nil && resp.Body != nil {
			err := resp.Body.Close()
			if err != nil {
				log.Println("关闭响应体错误：", err)
			}
		}
	}()

	var serverTimeResponse struct {
		ServerTime int64 `json:"serverTime"`
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("Error reading response body:", err)
		return 0
	}
	if err := json.Unmarshal(body, &serverTimeResponse); err != nil {
		log.Println("Error unmarshaling server time:", err)
		return 0
	}

	return serverTimeResponse.ServerTime
}

// 生成签名
func generateSignature(apiS string, params url.Values) string {
	// 将请求参数编码成 URL 格式的字符串
	queryString := params.Encode()

	// 生成签名
	mac := hmac.New(sha256.New, []byte(apiS))
	mac.Write([]byte(queryString)) // 用 API Secret 生成签名
	return hex.EncodeToString(mac.Sum(nil))
}

// BinanceResponse 包含多个仓位和账户信息
type BinanceResponse struct {
	Positions []*BinancePosition `json:"positions"` // 仓位信息
}

// getBinancePositionInfo 获取账户信息
func getBinancePositionInfo(apiK, apiS string) []*BinancePosition {
	// 请求的API地址
	endpoint := "/fapi/v2/account"
	baseURL := "https://fapi.binance.com"

	// 获取当前时间戳（使用服务器时间避免时差问题）
	serverTime := getBinanceServerTime()
	if serverTime == 0 {
		return nil
	}
	timestamp := strconv.FormatInt(serverTime, 10)

	// 设置请求参数
	params := url.Values{}
	params.Set("timestamp", timestamp)
	params.Set("recvWindow", "5000") // 设置接收窗口

	// 生成签名
	signature := generateSignature(apiS, params)

	// 将签名添加到请求参数中
	params.Set("signature", signature)

	// 构建完整的请求URL
	requestURL := baseURL + endpoint + "?" + params.Encode()

	// 创建请求
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		log.Println("Error creating request:", err)
		return nil
	}

	// 添加请求头
	req.Header.Add("X-MBX-APIKEY", apiK)

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("Error sending request:", err)
		return nil
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			err := resp.Body.Close()
			if err != nil {
				log.Println("关闭响应体错误：", err)
			}
		}
	}()

	// 读取响应
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("Error reading response:", err)
		return nil
	}

	// 解析响应
	var o *BinanceResponse
	err = json.Unmarshal(body, &o)
	if err != nil {
		log.Println("Error unmarshalling response:", err)
		return nil
	}

	// 返回资产余额
	return o.Positions
}

// CloseBinanceUserPositions close binance user positions
func (s *sBinanceTraderHistory) CloseBinanceUserPositions(ctx context.Context) uint64 {
	//var (
	//	err error
	//)
	//
	//for tmpI := int64(1); tmpI <= 1; tmpI++ {
	//
	//	tmpApiK := HandleKLineApiKey
	//	tmpApiS := HandleKLineApiSecret
	//	//if 2 == tmpI {
	//	//	tmpApiK = HandleKLineApiKeyTwo
	//	//	tmpApiS = HandleKLineApiSecretTwo
	//	//}
	//
	//	var (
	//		positions []*BinancePosition
	//	)
	//
	//	positions = getBinancePositionInfo(tmpApiK, tmpApiS)
	//	for _, v := range positions {
	//		// 新增
	//		var (
	//			currentAmount float64
	//		)
	//		currentAmount, err = strconv.ParseFloat(v.PositionAmt, 64)
	//		if nil != err {
	//			log.Println("close positions 获取用户仓位接口，解析出错", v)
	//			continue
	//		}
	//
	//		currentAmount = math.Abs(currentAmount)
	//		if floatEqual(currentAmount, 0, 1e-7) {
	//			continue
	//		}
	//
	//		var (
	//			symbolRel     = v.Symbol
	//			tmpQty        float64
	//			quantity      string
	//			quantityFloat float64
	//			orderType     = "MARKET"
	//			side          string
	//		)
	//		if "LONG" == v.PositionSide {
	//			side = "SELL"
	//		} else if "SHORT" == v.PositionSide {
	//			side = "BUY"
	//		} else {
	//			log.Println("close positions 仓位错误", v)
	//			continue
	//		}
	//
	//		tmpQty = currentAmount // 本次开单数量
	//		if !symbolsMap.Contains(symbolRel) {
	//			log.Println("close positions，代币信息无效，信息", v)
	//			continue
	//		}
	//
	//		// 精度调整
	//		if 0 >= symbolsMap.Get(symbolRel).(*entity.LhCoinSymbol).QuantityPrecision {
	//			quantity = fmt.Sprintf("%d", int64(tmpQty))
	//		} else {
	//			quantity = strconv.FormatFloat(tmpQty, 'f', symbolsMap.Get(symbolRel).(*entity.LhCoinSymbol).QuantityPrecision, 64)
	//		}
	//
	//		quantityFloat, err = strconv.ParseFloat(quantity, 64)
	//		if nil != err {
	//			log.Println("close positions，数量解析", v, err)
	//			continue
	//		}
	//
	//		if lessThanOrEqualZero(quantityFloat, 0, 1e-7) {
	//			continue
	//		}
	//
	//		var (
	//			binanceOrderRes *binanceOrder
	//			orderInfoRes    *orderInfo
	//		)
	//
	//		// 请求下单
	//		binanceOrderRes, orderInfoRes, err = requestBinanceOrder(symbolRel, side, orderType, v.PositionSide, quantity, tmpApiK, tmpApiS)
	//		if nil != err {
	//			log.Println("close positions，执行下单错误，手动：", err, symbolRel, side, orderType, v.PositionSide, quantity, tmpApiK, tmpApiS)
	//		}
	//
	//		// 下单异常
	//		if 0 >= binanceOrderRes.OrderId {
	//			log.Println("自定义下单，binance下单错误：", orderInfoRes)
	//			continue
	//		}
	//		log.Println("close, 执行成功：", v, binanceOrderRes)
	//
	//		time.Sleep(500 * time.Millisecond)
	//	}
	//}

	return 1
}

type binanceTradeHistoryResp struct {
	Data *binanceTradeHistoryData
}

type binanceTradeHistoryData struct {
	Total uint64
	List  []*binanceTradeHistoryDataList
}

type binanceTradeHistoryDataList struct {
	Time                uint64
	Symbol              string
	Side                string
	Price               float64
	Fee                 float64
	FeeAsset            string
	Quantity            float64
	QuantityAsset       string
	RealizedProfit      float64
	RealizedProfitAsset string
	BaseAsset           string
	Qty                 float64
	PositionSide        string
	ActiveBuy           bool
}

type binancePositionResp struct {
	Data []*binancePositionDataList
}

type binancePositionDataList struct {
	Symbol         string
	PositionSide   string
	PositionAmount string
	MarkPrice      string
}

type binancePositionHistoryResp struct {
	Data *binancePositionHistoryData
}

type binancePositionHistoryData struct {
	Total uint64
	List  []*binancePositionHistoryDataList
}

type binancePositionHistoryDataList struct {
	Time   uint64
	Symbol string
	Side   string
	Opened uint64
	Closed uint64
	Status string
}

type binanceTrade struct {
	TraderNum uint64
	Time      uint64
	Symbol    string
	Type      string
	Position  string
	Side      string
	Price     string
	Qty       string
	QtyFloat  float64
}

type Data struct {
	Symbol     string `json:"symbol"`
	Type       string `json:"type"`
	Price      string `json:"price"`
	Side       string `json:"side"`
	Qty        string `json:"qty"`
	Proportion string `json:"proportion"`
	Position   string `json:"position"`
}

type Order struct {
	Uid       uint64  `json:"uid"`
	BaseMoney string  `json:"base_money"`
	Data      []*Data `json:"data"`
	InitOrder uint64  `json:"init_order"`
	Rate      string  `json:"rate"`
	TraderNum uint64  `json:"trader_num"`
}

type SendBody struct {
	Orders    []*Order `json:"orders"`
	InitOrder uint64   `json:"init_order"`
}

type ListenTraderAndUserOrderRequest struct {
	SendBody SendBody `json:"send_body"`
}

type RequestResp struct {
	Status string
}

// 请求binance的下单历史接口
func (s *sBinanceTraderHistory) requestProxyBinanceTradeHistory(proxyAddr string, pageNumber int64, pageSize int64, portfolioId uint64) ([]*binanceTradeHistoryDataList, bool, error) {
	var (
		resp   *http.Response
		res    []*binanceTradeHistoryDataList
		b      []byte
		err    error
		apiUrl = "https://www.binance.com/bapi/futures/v1/friendly/future/copy-trade/lead-portfolio/trade-history"
	)

	proxy, err := url.Parse(proxyAddr)
	if err != nil {
		fmt.Println(err)
		return nil, true, err
	}
	netTransport := &http.Transport{
		Proxy:                 http.ProxyURL(proxy),
		MaxIdleConnsPerHost:   10,
		ResponseHeaderTimeout: time.Second * time.Duration(5),
	}
	httpClient := &http.Client{
		Timeout:   time.Second * 10,
		Transport: netTransport,
	}

	// 构造请求
	contentType := "application/json"
	data := `{"pageNumber":` + strconv.FormatInt(pageNumber, 10) + `,"pageSize":` + strconv.FormatInt(pageSize, 10) + `,portfolioId:` + strconv.FormatUint(portfolioId, 10) + `}`
	resp, err = httpClient.Post(apiUrl, contentType, strings.NewReader(data))
	if err != nil {
		fmt.Println(333, err)
		return nil, true, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(222, err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(111, err)
		return nil, true, err
	}

	var l *binanceTradeHistoryResp
	err = json.Unmarshal(b, &l)
	if err != nil {
		return nil, true, err
	}

	if nil == l.Data {
		return res, true, nil
	}

	res = make([]*binanceTradeHistoryDataList, 0)
	if nil == l.Data.List {
		return res, false, nil
	}

	res = make([]*binanceTradeHistoryDataList, 0)
	for _, v := range l.Data.List {
		res = append(res, v)
	}

	return res, false, nil
}

// 请求binance的仓位历史接口
func (s *sBinanceTraderHistory) requestProxyBinancePositionHistory(proxyAddr string, pageNumber int64, pageSize int64, portfolioId uint64) ([]*binancePositionHistoryDataList, bool, error) {
	var (
		resp   *http.Response
		res    []*binancePositionHistoryDataList
		b      []byte
		err    error
		apiUrl = "https://www.binance.com/bapi/futures/v1/friendly/future/copy-trade/lead-portfolio/position-history"
	)

	proxy, err := url.Parse(proxyAddr)
	if err != nil {
		fmt.Println(err)
		return nil, true, err
	}
	netTransport := &http.Transport{
		Proxy:                 http.ProxyURL(proxy),
		MaxIdleConnsPerHost:   10,
		ResponseHeaderTimeout: time.Second * time.Duration(5),
	}
	httpClient := &http.Client{
		Timeout:   time.Second * 10,
		Transport: netTransport,
	}

	// 构造请求
	contentType := "application/json"
	data := `{"sort":"OPENING","pageNumber":` + strconv.FormatInt(pageNumber, 10) + `,"pageSize":` + strconv.FormatInt(pageSize, 10) + `,portfolioId:` + strconv.FormatUint(portfolioId, 10) + `}`
	resp, err = httpClient.Post(apiUrl, contentType, strings.NewReader(data))
	if err != nil {
		fmt.Println(333, err)
		return nil, true, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(222, err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(111, err)
		return nil, true, err
	}

	var l *binancePositionHistoryResp
	err = json.Unmarshal(b, &l)
	if err != nil {
		return nil, true, err
	}

	if nil == l.Data {
		return res, true, nil
	}

	res = make([]*binancePositionHistoryDataList, 0)
	if nil == l.Data.List {
		return res, false, nil
	}

	res = make([]*binancePositionHistoryDataList, 0)
	for _, v := range l.Data.List {
		res = append(res, v)
	}

	return res, false, nil
}

// 请求binance的持有仓位历史接口，新
func (s *sBinanceTraderHistory) requestProxyBinancePositionHistoryNew(proxyAddr string, portfolioId uint64, cookie string, token string) ([]*binancePositionDataList, bool, error) {
	var (
		resp   *http.Response
		res    []*binancePositionDataList
		b      []byte
		err    error
		apiUrl = "https://www.binance.com/bapi/futures/v1/friendly/future/copy-trade/lead-data/positions?portfolioId=" + strconv.FormatUint(portfolioId, 10)
	)

	proxy, err := url.Parse(proxyAddr)
	if err != nil {
		fmt.Println(err)
		return nil, true, err
	}
	netTransport := &http.Transport{
		Proxy:                 http.ProxyURL(proxy),
		MaxIdleConnsPerHost:   10,
		ResponseHeaderTimeout: time.Second * time.Duration(5),
	}
	httpClient := &http.Client{
		Timeout:   time.Second * 2,
		Transport: netTransport,
	}

	// 构造请求
	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		fmt.Println("Error creating request:", err)
		fmt.Println(444444, err)
		return nil, true, err
	}

	// 添加头信息
	req.Header.Set("Clienttype", "web")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Csrftoken", token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0")

	// 构造请求
	resp, err = httpClient.Do(req)
	if err != nil {
		fmt.Println("Error making request:", err)
		fmt.Println(444444, err)
		return nil, true, err
	}

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(4444, err)
		return nil, true, err
	}

	var l *binancePositionResp
	err = json.Unmarshal(b, &l)
	if err != nil {
		return nil, true, err
	}

	if nil == l.Data {
		return res, true, nil
	}

	res = make([]*binancePositionDataList, 0)
	for _, v := range l.Data {
		res = append(res, v)
	}

	return res, false, nil
}

// 请求binance的持有仓位历史接口，新
func (s *sBinanceTraderHistory) requestBinancePositionHistoryNew(portfolioId uint64, cookie string, token string) ([]*binancePositionDataList, bool, error) {
	var (
		resp   *http.Response
		res    []*binancePositionDataList
		b      []byte
		err    error
		apiUrl = "https://www.binance.com/bapi/futures/v1/friendly/future/copy-trade/lead-data/positions?portfolioId=" + strconv.FormatUint(portfolioId, 10)
	)

	// 创建不验证 SSL 证书的 HTTP 客户端
	httpClient := &http.Client{
		Timeout: time.Second * 2,
	}

	// 构造请求
	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		fmt.Println("Error creating request:", err)
		return nil, true, err
	}

	// 添加头信息
	req.Header.Set("Clienttype", "web")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Csrftoken", token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0")

	// 发送请求
	resp, err = httpClient.Do(req)
	if err != nil {
		fmt.Println("Error making request:", err)
		return nil, true, err
	}

	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(44444, err)
		}
	}(resp.Body)

	// 结果
	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(4444, err)
		return nil, true, err
	}

	//fmt.Println(string(b))
	var l *binancePositionResp
	err = json.Unmarshal(b, &l)
	if err != nil {
		return nil, true, err
	}

	if nil == l.Data {
		return res, true, nil
	}

	res = make([]*binancePositionDataList, 0)
	for _, v := range l.Data {
		res = append(res, v)
	}

	return res, false, nil
}

type binanceOrder struct {
	OrderId       int64
	ExecutedQty   string
	ClientOrderId string
	Symbol        string
	AvgPrice      string
	CumQuote      string
	Side          string
	PositionSide  string
	ClosePosition bool
	Type          string
	Status        string
}

type orderInfo struct {
	Code int64
	Msg  string
}

func requestBinanceOrder(symbol string, side string, orderType string, positionSide string, quantity string, apiKey string, secretKey string) (*binanceOrder, *orderInfo, error) {
	var (
		client       *http.Client
		req          *http.Request
		resp         *http.Response
		res          *binanceOrder
		resOrderInfo *orderInfo
		data         string
		b            []byte
		err          error
		apiUrl       = "https://fapi.binance.com/fapi/v1/order"
	)

	//fmt.Println(symbol, side, orderType, positionSide, quantity, apiKey, secretKey)
	// 时间
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// 拼请求数据
	data = "symbol=" + symbol + "&side=" + side + "&type=" + orderType + "&positionSide=" + positionSide + "&newOrderRespType=" + "RESULT" + "&quantity=" + quantity + "&timestamp=" + now

	// 加密
	h := hmac.New(sha256.New, []byte(secretKey))
	h.Write([]byte(data))
	signature := hex.EncodeToString(h.Sum(nil))
	// 构造请求

	req, err = http.NewRequest("POST", apiUrl, strings.NewReader(data+"&signature="+signature))
	if err != nil {
		return nil, nil, err
	}
	// 添加头信息
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-MBX-APIKEY", apiKey)

	// 请求执行
	client = &http.Client{Timeout: 3 * time.Second}
	resp, err = client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	var o binanceOrder
	err = json.Unmarshal(b, &o)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	res = &binanceOrder{
		OrderId:       o.OrderId,
		ExecutedQty:   o.ExecutedQty,
		ClientOrderId: o.ClientOrderId,
		Symbol:        o.Symbol,
		AvgPrice:      o.AvgPrice,
		CumQuote:      o.CumQuote,
		Side:          o.Side,
		PositionSide:  o.PositionSide,
		ClosePosition: o.ClosePosition,
		Type:          o.Type,
	}

	if 0 >= res.OrderId {
		//fmt.Println(string(b))
		err = json.Unmarshal(b, &resOrderInfo)
		if err != nil {
			fmt.Println(string(b), err)
			return nil, nil, err
		}
	}

	return res, resOrderInfo, nil
}

func requestBinanceOrderStop(symbol string, side string, positionSide string, quantity string, stopPrice string, price string, apiKey string, secretKey string) (*binanceOrder, *orderInfo, error) {
	//fmt.Println(symbol, side, positionSide, quantity, stopPrice, price, apiKey, secretKey)
	var (
		client       *http.Client
		req          *http.Request
		resp         *http.Response
		res          *binanceOrder
		resOrderInfo *orderInfo
		data         string
		b            []byte
		err          error
		apiUrl       = "https://fapi.binance.com/fapi/v1/order"
	)

	// 时间
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// 拼请求数据
	data = "symbol=" + symbol + "&side=" + side + "&type=STOP_MARKET&stopPrice=" + stopPrice + "&positionSide=" + positionSide + "&newOrderRespType=" + "RESULT" + "&quantity=" + quantity + "&timestamp=" + now

	// 加密
	h := hmac.New(sha256.New, []byte(secretKey))
	h.Write([]byte(data))
	signature := hex.EncodeToString(h.Sum(nil))
	// 构造请求

	req, err = http.NewRequest("POST", apiUrl, strings.NewReader(data+"&signature="+signature))
	if err != nil {
		return nil, nil, err
	}
	// 添加头信息
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-MBX-APIKEY", apiKey)

	// 请求执行
	client = &http.Client{Timeout: 3 * time.Second}
	resp, err = client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	var o binanceOrder
	err = json.Unmarshal(b, &o)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	res = &binanceOrder{
		OrderId:       o.OrderId,
		ExecutedQty:   o.ExecutedQty,
		ClientOrderId: o.ClientOrderId,
		Symbol:        o.Symbol,
		AvgPrice:      o.AvgPrice,
		CumQuote:      o.CumQuote,
		Side:          o.Side,
		PositionSide:  o.PositionSide,
		ClosePosition: o.ClosePosition,
		Type:          o.Type,
	}

	if 0 >= res.OrderId {
		//fmt.Println(string(b))
		err = json.Unmarshal(b, &resOrderInfo)
		if err != nil {
			fmt.Println(string(b), err)
			return nil, nil, err
		}
	}

	return res, resOrderInfo, nil
}

func requestBinanceOrderStopTakeProfit(symbol string, side string, positionSide string, quantity string, stopPrice string, price string, apiKey string, secretKey string) (*binanceOrder, *orderInfo, error) {
	//fmt.Println(symbol, side, positionSide, quantity, stopPrice, price, apiKey, secretKey)
	var (
		client       *http.Client
		req          *http.Request
		resp         *http.Response
		res          *binanceOrder
		resOrderInfo *orderInfo
		data         string
		b            []byte
		err          error
		apiUrl       = "https://fapi.binance.com/fapi/v1/order"
	)

	// 时间
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// 拼请求数据
	data = "symbol=" + symbol + "&side=" + side + "&type=TAKE_PROFIT&stopPrice=" + stopPrice + "&price=" + price + "&positionSide=" + positionSide + "&newOrderRespType=" + "RESULT" + "&quantity=" + quantity + "&timestamp=" + now

	// 加密
	h := hmac.New(sha256.New, []byte(secretKey))
	h.Write([]byte(data))
	signature := hex.EncodeToString(h.Sum(nil))
	// 构造请求

	req, err = http.NewRequest("POST", apiUrl, strings.NewReader(data+"&signature="+signature))
	if err != nil {
		return nil, nil, err
	}
	// 添加头信息
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-MBX-APIKEY", apiKey)

	// 请求执行
	client = &http.Client{Timeout: 3 * time.Second}
	resp, err = client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	var o binanceOrder
	err = json.Unmarshal(b, &o)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	res = &binanceOrder{
		OrderId:       o.OrderId,
		ExecutedQty:   o.ExecutedQty,
		ClientOrderId: o.ClientOrderId,
		Symbol:        o.Symbol,
		AvgPrice:      o.AvgPrice,
		CumQuote:      o.CumQuote,
		Side:          o.Side,
		PositionSide:  o.PositionSide,
		ClosePosition: o.ClosePosition,
		Type:          o.Type,
	}

	if 0 >= res.OrderId {
		//fmt.Println(string(b))
		err = json.Unmarshal(b, &resOrderInfo)
		if err != nil {
			fmt.Println(string(b), err)
			return nil, nil, err
		}
	}

	return res, resOrderInfo, nil
}

type BinanceTraderDetailResp struct {
	Data *BinanceTraderDetailData
}

type BinanceTraderDetailData struct {
	MarginBalance string
}

// 拉取交易员交易历史
func requestBinanceTraderDetail(portfolioId uint64) (string, error) {
	var (
		resp   *http.Response
		res    string
		b      []byte
		err    error
		apiUrl = "https://www.binance.com/bapi/futures/v1/friendly/future/copy-trade/lead-portfolio/detail?portfolioId=" + strconv.FormatUint(portfolioId, 10)
	)

	// 构造请求
	resp, err = http.Get(apiUrl)
	if err != nil {
		return res, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return res, err
	}

	var l *BinanceTraderDetailResp
	err = json.Unmarshal(b, &l)
	if err != nil {
		fmt.Println(err)
		return res, err
	}

	if nil == l.Data {
		return res, nil
	}

	return l.Data.MarginBalance, nil
}

type BinanceTraderExchangeInfoResp struct {
	Symbols []*BinanceExchangeInfoSymbol
}

type BinanceExchangeInfoSymbol struct {
	Symbol  string
	Filters []*BinanceExchangeInfoSymbolFilter
}

type BinanceExchangeInfoSymbolFilter struct {
	TickSize   string
	FilterType string
}

// 拉取币种信息
func requestBinanceExchangeInfo() ([]*BinanceExchangeInfoSymbol, error) {
	var (
		resp   *http.Response
		res    []*BinanceExchangeInfoSymbol
		b      []byte
		err    error
		apiUrl = "https://fapi.binance.com/fapi/v1/exchangeInfo"
	)

	// 构造请求
	resp, err = http.Get(apiUrl)
	if err != nil {
		return res, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return res, err
	}

	var l *BinanceTraderExchangeInfoResp
	err = json.Unmarshal(b, &l)
	if err != nil {
		fmt.Println(err)
		return res, err
	}

	if nil == l.Symbols || 0 >= len(l.Symbols) {
		return res, nil
	}

	return l.Symbols, nil
}

// 撤销订单信息
func requestBinanceDeleteOrder(symbol string, orderId string, apiKey string, secretKey string) (*binanceOrder, *orderInfo, error) {
	var (
		client       *http.Client
		req          *http.Request
		resp         *http.Response
		res          *binanceOrder
		resOrderInfo *orderInfo
		data         string
		b            []byte
		err          error
		apiUrl       = "https://fapi.binance.com/fapi/v1/order"
	)

	// 时间
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// 拼请求数据
	data = "symbol=" + symbol + "&orderId=" + orderId + "&timestamp=" + now

	// 加密
	h := hmac.New(sha256.New, []byte(secretKey))
	h.Write([]byte(data))
	signature := hex.EncodeToString(h.Sum(nil))
	// 构造请求

	req, err = http.NewRequest("DELETE", apiUrl, strings.NewReader(data+"&signature="+signature))
	if err != nil {
		return nil, nil, err
	}
	// 添加头信息
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-MBX-APIKEY", apiKey)

	// 请求执行
	client = &http.Client{Timeout: 3 * time.Second}
	resp, err = client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	var o binanceOrder
	err = json.Unmarshal(b, &o)
	if err != nil {
		fmt.Println(string(b), err)
		return nil, nil, err
	}

	res = &binanceOrder{
		OrderId:       o.OrderId,
		ExecutedQty:   o.ExecutedQty,
		ClientOrderId: o.ClientOrderId,
		Symbol:        o.Symbol,
		AvgPrice:      o.AvgPrice,
		CumQuote:      o.CumQuote,
		Side:          o.Side,
		PositionSide:  o.PositionSide,
		ClosePosition: o.ClosePosition,
		Type:          o.Type,
	}

	if 0 >= res.OrderId {
		//fmt.Println(string(b))
		err = json.Unmarshal(b, &resOrderInfo)
		if err != nil {
			fmt.Println(string(b), err)
			return nil, nil, err
		}
	}

	return res, resOrderInfo, nil
}

func requestBinanceOrderInfo(symbol string, orderId string, apiKey string, secretKey string) (*binanceOrder, error) {
	var (
		client *http.Client
		req    *http.Request
		resp   *http.Response
		res    *binanceOrder
		data   string
		b      []byte
		err    error
		apiUrl = "https://fapi.binance.com/fapi/v1/order"
	)

	// 时间
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// 拼请求数据
	data = "symbol=" + symbol + "&orderId=" + orderId + "&timestamp=" + now
	// 加密
	h := hmac.New(sha256.New, []byte(secretKey))
	h.Write([]byte(data))
	signature := hex.EncodeToString(h.Sum(nil))
	// 构造请求

	req, err = http.NewRequest("GET", apiUrl, strings.NewReader(data+"&signature="+signature))
	if err != nil {
		return nil, err
	}
	// 添加头信息
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-MBX-APIKEY", apiKey)

	// 请求执行
	client = &http.Client{Timeout: 3 * time.Second}
	resp, err = client.Do(req)
	if err != nil {
		return nil, err
	}

	// 结果
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	var o binanceOrder
	err = json.Unmarshal(b, &o)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	res = &binanceOrder{
		OrderId:       o.OrderId,
		ExecutedQty:   o.ExecutedQty,
		ClientOrderId: o.ClientOrderId,
		Symbol:        o.Symbol,
		AvgPrice:      o.AvgPrice,
		CumQuote:      o.CumQuote,
		Side:          o.Side,
		PositionSide:  o.PositionSide,
		ClosePosition: o.ClosePosition,
		Type:          o.Type,
		Status:        o.Status,
	}

	return res, nil
}

// BinanceExchangeInfoResp 结构体表示 Binance 交易对信息的 API 响应
type BinanceExchangeInfoResp struct {
	Symbols []*BinanceSymbolInfo `json:"symbols"`
}

// BinanceSymbolInfo 结构体表示单个交易对的信息
type BinanceSymbolInfo struct {
	Symbol            string `json:"symbol"`
	Pair              string `json:"pair"`
	ContractType      string `json:"contractType"`
	Status            string `json:"status"`
	BaseAsset         string `json:"baseAsset"`
	QuoteAsset        string `json:"quoteAsset"`
	MarginAsset       string `json:"marginAsset"`
	PricePrecision    int    `json:"pricePrecision"`
	QuantityPrecision int    `json:"quantityPrecision"`
}

// 获取 Binance U 本位合约交易对信息
func getBinanceFuturesPairs() ([]*BinanceSymbolInfo, error) {
	apiUrl := "https://fapi.binance.com/fapi/v1/exchangeInfo"

	// 发送 HTTP GET 请求
	resp, err := http.Get(apiUrl)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			err := resp.Body.Close()
			if err != nil {
				log.Println("关闭响应体错误：", err)
			}
		}
	}()

	// 读取响应体
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 解析 JSON 响应
	var exchangeInfo *BinanceExchangeInfoResp
	err = json.Unmarshal(body, &exchangeInfo)
	if err != nil {
		return nil, err
	}

	return exchangeInfo.Symbols, nil
}

// GetGateContract 获取合约账号信息
func getGateContract() ([]gateapi.Contract, error) {
	client := gateapi.NewAPIClient(gateapi.NewConfiguration())
	// uncomment the next line if your are testing against testnet
	// client.ChangeBasePath("https://fx-api-testnet.gateio.ws/api/v4")
	ctx := context.WithValue(context.Background(),
		gateapi.ContextGateAPIV4,
		gateapi.GateAPIV4{},
	)

	result, _, err := client.FuturesApi.ListFuturesContracts(ctx, "usdt", &gateapi.ListFuturesContractsOpts{})
	if err != nil {
		var e gateapi.GateAPIError
		if errors.As(err, &e) {
			log.Println("gate api error: ", e.Error())
			return result, err
		}
	}

	return result, nil
}

// PlaceOrderGate places an order on the Gate.io API with dynamic parameters
func placeOrderGate(apiK, apiS, contract string, size int64, reduceOnly bool, autoSize string) (gateapi.FuturesOrder, error) {
	client := gateapi.NewAPIClient(gateapi.NewConfiguration())
	// uncomment the next line if your are testing against testnet
	// client.ChangeBasePath("https://fx-api-testnet.gateio.ws/api/v4")
	ctx := context.WithValue(context.Background(),
		gateapi.ContextGateAPIV4,
		gateapi.GateAPIV4{
			Key:    apiK,
			Secret: apiS,
		},
	)

	order := gateapi.FuturesOrder{
		Contract: contract,
		Size:     size,
		Tif:      "ioc",
		Price:    "0",
	}

	if autoSize != "" {
		order.AutoSize = autoSize
	}

	// 如果 reduceOnly 为 true，添加到请求数据中
	if reduceOnly {
		order.ReduceOnly = reduceOnly
	}

	result, _, err := client.FuturesApi.CreateFuturesOrder(ctx, "usdt", order)

	if err != nil {
		var e gateapi.GateAPIError
		if errors.As(err, &e) {
			log.Println("gate api error: ", e.Error())
			return result, err
		}
	}

	return result, nil
}

// PlaceOrderGate places an order on the Gate.io API with dynamic parameters
func placeLimitCloseOrderGate(apiK, apiS, contract string, price string, autoSize string) (gateapi.FuturesOrder, error) {
	client := gateapi.NewAPIClient(gateapi.NewConfiguration())
	// uncomment the next line if your are testing against testnet
	// client.ChangeBasePath("https://fx-api-testnet.gateio.ws/api/v4")
	ctx := context.WithValue(context.Background(),
		gateapi.ContextGateAPIV4,
		gateapi.GateAPIV4{
			Key:    apiK,
			Secret: apiS,
		},
	)

	order := gateapi.FuturesOrder{
		Contract:     contract,
		Size:         0,
		Price:        price,
		Tif:          "gtc",
		ReduceOnly:   true,
		AutoSize:     autoSize,
		IsReduceOnly: true,
		IsClose:      true,
	}

	result, _, err := client.FuturesApi.CreateFuturesOrder(ctx, "usdt", order)

	if err != nil {
		var e gateapi.GateAPIError
		if errors.As(err, &e) {
			log.Println("gate api error: ", e.Error())
			return result, err
		}
	}

	return result, nil
}

// PlaceOrderGate places an order on the Gate.io API with dynamic parameters
func removeLimitCloseOrderGate(apiK, apiS, orderId string) (gateapi.FuturesOrder, error) {
	client := gateapi.NewAPIClient(gateapi.NewConfiguration())
	// uncomment the next line if your are testing against testnet
	// client.ChangeBasePath("https://fx-api-testnet.gateio.ws/api/v4")
	ctx := context.WithValue(context.Background(),
		gateapi.ContextGateAPIV4,
		gateapi.GateAPIV4{
			Key:    apiK,
			Secret: apiS,
		},
	)

	result, _, err := client.FuturesApi.CancelFuturesOrder(ctx, "usdt", orderId)

	if err != nil {
		var e gateapi.GateAPIError
		if errors.As(err, &e) {
			log.Println("gate api error: ", e.Error())
			return result, err
		}
	}

	return result, nil
}

// PlaceOrderGate places an order on the Gate.io API with dynamic parameters
func getOrderGate(apiK, apiS, orderId string) (gateapi.FuturesOrder, error) {
	client := gateapi.NewAPIClient(gateapi.NewConfiguration())
	// uncomment the next line if your are testing against testnet
	// client.ChangeBasePath("https://fx-api-testnet.gateio.ws/api/v4")
	ctx := context.WithValue(context.Background(),
		gateapi.ContextGateAPIV4,
		gateapi.GateAPIV4{
			Key:    apiK,
			Secret: apiS,
		},
	)

	result, _, err := client.FuturesApi.GetFuturesOrder(ctx, "usdt", orderId)

	if err != nil {
		var e gateapi.GateAPIError
		if errors.As(err, &e) {
			log.Println("gate api error: ", e.Error())
			return result, err
		}
	}

	return result, nil
}

func placeLimitOrderGate(apiK, apiS, contract string, rule, timeLimit int32, price string, autoSize string) (gateapi.TriggerOrderResponse, error) {
	client := gateapi.NewAPIClient(gateapi.NewConfiguration())
	ctx := context.WithValue(context.Background(),
		gateapi.ContextGateAPIV4,
		gateapi.GateAPIV4{
			Key:    apiK,
			Secret: apiS,
		},
	)

	order := gateapi.FuturesPriceTriggeredOrder{
		Initial: gateapi.FuturesInitialOrder{
			Contract:     contract,
			Size:         0,
			Price:        price,
			Tif:          "gtc",
			ReduceOnly:   true,
			AutoSize:     autoSize,
			IsReduceOnly: true,
			IsClose:      true,
		},
		Trigger: gateapi.FuturesPriceTrigger{
			StrategyType: 0,
			PriceType:    0,
			Price:        price,
			Rule:         rule,
			Expiration:   timeLimit,
		},
	}

	result, _, err := client.FuturesApi.CreatePriceTriggeredOrder(ctx, "usdt", order)

	if err != nil {
		var e gateapi.GateAPIError
		if errors.As(err, &e) {
			log.Println("gate api error: ", e.Error())
			return result, err
		}
		return result, err
	}

	return result, nil
}

type KLineMOne struct {
	ID                  int64
	StartTime           int64
	EndTime             int64
	StartPrice          float64
	TopPrice            float64
	LowPrice            float64
	EndPrice            float64
	DealTotalAmount     float64
	DealAmount          float64
	DealTotal           int64
	DealSelfTotalAmount float64
	DealSelfAmount      float64
}

type KLineU struct {
	OpenTime               int64
	Open, High, Low, Close string
	Volume                 string
	CloseTime              int64
	QuoteAssetVolume       string
	TradeNum               int
	TakerBuyBaseVolume     string
	TakerBuyQuoteVolume    string
	Ignore                 string
}

// 请求 Binance U 本位合约 K 线数据
func requestBinanceFuturesKLines(symbol, interval, startTime, endTime, limit string) ([]*KLineU, error) {
	apiUrl := "https://fapi.binance.com/fapi/v1/klines"

	// 参数
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("interval", interval) // 如 15m、1h、4h、1d 等
	if startTime != "" {
		params.Set("startTime", startTime)
	}
	if endTime != "" {
		params.Set("endTime", endTime)
	}
	if limit != "" {
		params.Set("limit", limit)
	}

	// 构建完整URL
	u, err := url.ParseRequestURI(apiUrl)
	if err != nil {
		return nil, err
	}
	u.RawQuery = params.Encode()

	// 请求
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		errTwo := Body.Close()
		if errTwo != nil {

		}
	}(resp.Body)

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 解析
	var rawData [][]interface{}
	err = json.Unmarshal(body, &rawData)
	if err != nil {
		return nil, err
	}

	// 转换为结构体
	var result []*KLineU
	for _, item := range rawData {
		result = append(result, &KLineU{
			OpenTime:            int64(item[0].(float64)),
			Open:                item[1].(string),
			High:                item[2].(string),
			Low:                 item[3].(string),
			Close:               item[4].(string),
			Volume:              item[5].(string),
			CloseTime:           int64(item[6].(float64)),
			QuoteAssetVolume:    item[7].(string),
			TradeNum:            int(item[8].(float64)),
			TakerBuyBaseVolume:  item[9].(string),
			TakerBuyQuoteVolume: item[10].(string),
			Ignore:              item[11].(string),
		})
	}

	return result, nil
}

type KLineDay struct {
	OpenTime               int64
	Open, High, Low, Close string
	Volume                 string
	CloseTime              int64
	QuoteAssetVolume       string
	TradeNum               int
	TakerBuyBaseVolume     string
	TakerBuyQuoteVolume    string
	Ignore                 string
}

func requestBinanceDailyKLines(symbol, interval, startTime, endTime string, limit string) ([]*KLineDay, error) {
	apiUrl := "https://api.binance.com/api/v3/klines"

	// 参数
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("interval", interval) // 日线
	if startTime != "" {
		params.Set("startTime", startTime)
	}
	if endTime != "" {
		params.Set("endTime", endTime)
	}
	if limit != "" {
		params.Set("limit", limit)
	}

	// 构建完整URL
	u, err := url.ParseRequestURI(apiUrl)
	if err != nil {
		return nil, err
	}
	u.RawQuery = params.Encode()

	// 请求
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		errTwo := Body.Close()
		if errTwo != nil {

		}
	}(resp.Body)

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 解析
	var rawData [][]interface{}
	err = json.Unmarshal(body, &rawData)
	if err != nil {
		return nil, err
	}

	// 转换成结构体
	var result []*KLineDay
	for _, item := range rawData {
		result = append(result, &KLineDay{
			OpenTime:            int64(item[0].(float64)),
			Open:                item[1].(string),
			High:                item[2].(string),
			Low:                 item[3].(string),
			Close:               item[4].(string),
			Volume:              item[5].(string),
			CloseTime:           int64(item[6].(float64)),
			QuoteAssetVolume:    item[7].(string),
			TradeNum:            int(item[8].(float64)),
			TakerBuyBaseVolume:  item[9].(string),
			TakerBuyQuoteVolume: item[10].(string),
			Ignore:              item[11].(string),
		})
	}

	return result, nil
}

type ExchangeInfo struct {
	Timezone   string   `json:"timezone"`
	ServerTime int64    `json:"serverTime"`
	Symbols    []Symbol `json:"symbols"`
}

type Symbol struct {
	Symbol             string   `json:"symbol"`
	Status             string   `json:"status"`
	BaseAsset          string   `json:"baseAsset"`
	BaseAssetPrecision int      `json:"baseAssetPrecision"`
	QuoteAsset         string   `json:"quoteAsset"`
	QuotePrecision     int      `json:"quotePrecision"`
	OrderTypes         []string `json:"orderTypes"`
	IsSpotTrading      bool     `json:"isSpotTradingAllowed"`
	IsMarginTrading    bool     `json:"isMarginTradingAllowed"`
}

func getBinanceExchangeInfo() (*ExchangeInfo, error) {
	url := "https://api.binance.com/api/v3/exchangeInfo"

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 解析 JSON
	var info ExchangeInfo
	err = json.Unmarshal(body, &info)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

// 结构体定义
type FuturesPrice struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

// 请求函数
func getUSDMFuturesPrice(symbol string) (*FuturesPrice, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/ticker/price?symbol=%s", symbol)

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var price FuturesPrice
	err = json.Unmarshal(body, &price)
	if err != nil {
		return nil, err
	}

	return &price, nil
}

// 获取所有 U 本位合约的当前价格
func getAllUSDMFuturesPrices() (map[string]float64, error) {
	url := "https://fapi.binance.com/fapi/v1/ticker/price"

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var prices []FuturesPrice
	err = json.Unmarshal(body, &prices)
	if err != nil {
		return nil, err
	}

	// 转换为 map：symbol -> price(float64)
	priceMap := make(map[string]float64)
	for _, p := range prices {
		var f float64
		f, err = strconv.ParseFloat(p.Price, 10)
		if err != nil {
			continue
		}

		if 0 >= f {
			fmt.Println("价格0，usdt")
			continue
		}

		priceMap[p.Symbol] = f
	}

	return priceMap, nil
}
