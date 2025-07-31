// ================================================================================
// Code generated and maintained by GoFrame CLI tool. DO NOT EDIT.
// You can delete these comments if you wish manually maintain this interface file.
// ================================================================================

package service

import (
	"binance_data_gf/internal/model/entity"
	"context"
)

type (
	IBinanceTraderHistory interface {
		DeleteUser(user *entity.NewUser) bool
		UpdateUser(user *entity.NewUser) bool
		GetUsers() []*entity.UserInfo
		// InsertUser 初始化信息
		InsertUser(userP *entity.NewUser) bool
		// UpdateCoinInfo 初始化信息
		UpdateCoinInfo(ctx context.Context) bool
		// HandleKLineNew .
		HandleKLineNew(ctx context.Context)
		// CloseBinanceUserPositions close binance user positions
		CloseBinanceUserPositions(ctx context.Context) uint64
	}
)

var (
	localBinanceTraderHistory IBinanceTraderHistory
)

func BinanceTraderHistory() IBinanceTraderHistory {
	if localBinanceTraderHistory == nil {
		panic("implement not found for interface IBinanceTraderHistory, forgot register?")
	}
	return localBinanceTraderHistory
}

func RegisterBinanceTraderHistory(i IBinanceTraderHistory) {
	localBinanceTraderHistory = i
}
