package cmd

import (
	"binance_data_gf/internal/model/entity"
	"binance_data_gf/internal/service"
	"context"
	"fmt"
	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/os/gcmd"
	"github.com/gogf/gf/v2/os/gtimer"
	"strconv"
	"time"
)

var (
	Main = &gcmd.Command{
		Name: "main",
	}

	// TraderGuiNew 监听系统中指定的交易员-龟兔赛跑
	TraderGuiNew = &gcmd.Command{
		Name:  "traderGuiNew",
		Brief: "listen trader",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			serviceBinanceTrader := service.BinanceTraderHistory()
			// 初始化
			if !serviceBinanceTrader.UpdateCoinInfo(ctx) {
				fmt.Println("初始化币种失败，fail")
				return nil
			}
			fmt.Println("初始化币种成功，ok")

			// 300秒/次，币种信息
			handle3 := func(ctx context.Context) {
				serviceBinanceTrader.UpdateCoinInfo(ctx)
			}
			gtimer.AddSingleton(ctx, time.Second*300, handle3)

			// 开启http管理服务
			s := g.Server()
			s.Group("/api", func(group *ghttp.RouterGroup) {
				group.Middleware(func(r *ghttp.Request) {
					r.Response.Header().Set("Access-Control-Allow-Origin", "*")
					r.Response.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
					r.Response.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

					// OPTIONS 请求直接返回
					if r.Method == "OPTIONS" {
						r.Exit()
					}
					r.Middleware.Next()
				})

				// 查询num
				group.GET("/handle_k_line", func(r *ghttp.Request) {
					serviceBinanceTrader.HandleKLineNew(ctx)
					return
				})

				group.POST("/insert_user", func(r *ghttp.Request) {
					var (
						num      float64
						first    float64
						second   float64
						parseErr error
					)
					num, parseErr = strconv.ParseFloat(r.PostFormValue("num"), 64)
					if nil != parseErr || 0 >= num {
						r.Response.WriteJson(g.Map{
							"code": -1,
						})

						return
					}

					first, parseErr = strconv.ParseFloat(r.PostFormValue("first"), 64)
					if nil != parseErr || 0 >= first {
						r.Response.WriteJson(g.Map{
							"code": -1,
						})

						return
					}

					second, parseErr = strconv.ParseFloat(r.PostFormValue("second"), 64)
					if nil != parseErr || 0 >= second {
						r.Response.WriteJson(g.Map{
							"code": -1,
						})

						return
					}

					res := serviceBinanceTrader.InsertUser(&entity.NewUser{
						ApiKey:    r.PostFormValue("apiKey"),
						ApiSecret: r.PostFormValue("apiSecret"),
						Num:       num,
						First:     first,
						Second:    second,
					})

					if res {
						r.Response.WriteJson(g.Map{
							"code": 1,
						})
					} else {
						r.Response.WriteJson(g.Map{
							"code": -2,
						})
					}

					return
				})

				group.POST("/update_user", func(r *ghttp.Request) {
					var (
						num      float64
						first    float64
						second   float64
						parseErr error
					)
					num, parseErr = strconv.ParseFloat(r.PostFormValue("num"), 64)
					if nil != parseErr || 0 >= num {
						r.Response.WriteJson(g.Map{
							"code": -1,
						})

						return
					}

					first, parseErr = strconv.ParseFloat(r.PostFormValue("first"), 64)
					if nil != parseErr || 0 >= first {
						r.Response.WriteJson(g.Map{
							"code": -1,
						})

						return
					}

					second, parseErr = strconv.ParseFloat(r.PostFormValue("second"), 64)
					if nil != parseErr || 0 >= second {
						r.Response.WriteJson(g.Map{
							"code": -1,
						})

						return
					}

					res := serviceBinanceTrader.UpdateUser(&entity.NewUser{
						ApiKey: r.PostFormValue("apiKey"),
						Num:    num,
						First:  first,
						Second: second,
					})

					if res {
						r.Response.WriteJson(g.Map{
							"code": 1,
						})
					} else {
						r.Response.WriteJson(g.Map{
							"code": -2,
						})
					}

					return
				})

				group.POST("/delete_user", func(r *ghttp.Request) {
					res := serviceBinanceTrader.DeleteUser(&entity.NewUser{
						ApiKey: r.PostFormValue("apiKey"),
					})

					if res {
						r.Response.WriteJson(g.Map{
							"code": 1,
						})
					} else {
						r.Response.WriteJson(g.Map{
							"code": -2,
						})
					}
					return
				})
			})

			s.SetPort(80)
			s.Run()

			// 阻塞
			select {}
		},
	}
)
