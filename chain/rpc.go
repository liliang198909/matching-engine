package chain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// RPCSource 用 go-ethereum 的 ethclient 实现 Source，直接连 JSON-RPC 节点。
//
// 整个包里只有这一处引入 go-ethereum 的客户端，
// watcher 和测试都只依赖 Source 接口，所以换节点实现（自建、第三方托管）
// 不需要改任何业务代码。
type RPCSource struct {
	client *ethclient.Client
	token  common.Address
}

// NewRPCSource 绑定一个节点连接与一个 ERC20 合约地址。
func NewRPCSource(client *ethclient.Client, token common.Address) *RPCSource {
	return &RPCSource{client: client, token: token}
}

// BlockNumber 返回节点当前的最新区块高度。
func (s *RPCSource) BlockNumber(ctx context.Context) (uint64, error) {
	return s.client.BlockNumber(ctx)
}

// FilterLogs 拉取指定区块区间内该合约的 Transfer 事件。
//
// 在客户端就带上地址与 topic 过滤条件，而不是把区间内所有日志拉回来再筛：
// 一个区块里可能有成百上千条日志，交给节点过滤能省掉绝大部分传输量。
func (s *RPCSource) FilterLogs(ctx context.Context, from, to uint64) ([]types.Log, error) {
	return s.client.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{s.token},
		Topics:    [][]common.Hash{{TransferTopic}},
	})
}

// ParseToken 校验并解析 ERC20 合约地址。
//
// 在启动阶段就校验，而不是第一次拉日志时才让节点报错：
// 配置写错属于部署期问题，应当在进程起不来的时候暴露。
func ParseToken(hex string) (common.Address, error) {
	if !common.IsHexAddress(hex) {
		return common.Address{}, fmt.Errorf("invalid contract address %q", hex)
	}
	return common.HexToAddress(hex), nil
}
