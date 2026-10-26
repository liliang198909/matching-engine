// Package chain 订阅链上 ERC20 合约的 Transfer 事件，并把结果交给本地存储。
//
// 它只做三件事：取区块、拉日志、解码。不碰数据库、不碰 HTTP，
// 因此可以在没有任何链的前提下用假节点完整测试。
package chain

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// TransferTopic 是 ERC20 Transfer(address,address,uint256) 的事件签名哈希。
//
// 一个合约上可能有很多事件；logs 的第一个 topic 永远是事件签名，
// 用它就能把 Transfer 和 Approval 等其他事件区分开，客户端不需要完整 ABI。
var TransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// Transfer 是一条已解码的 ERC20 转账记录。
type Transfer struct {
	Token       string // 合约地址，统一小写
	BlockNumber uint64 // 所在区块高度
	TxHash      string // 所属交易哈希
	LogIndex    uint   // 该日志在区块内的序号，与 TxHash 一起唯一确定一条记录
	From        string // 转出地址
	To          string // 转入地址
	Value       string // 金额的十进制字符串
}

// DecodeTransfer 把一条原始日志解码成 Transfer。
//
// 第二个返回值为 false 表示这不是一条 Transfer 事件（例如 Approval），
// 调用方应当跳过而不是把它当成错误——同一合约上的无关事件很常见。
//
// 金额用十进制字符串而不是数字：ERC20 的金额是 256 位整数，
// 换成 float64 会在 2^53 之后就开始丢精度，换成 int64 则直接溢出。
// 数据库里同样存字符串，避免在「链 → 内存 → 库 → JSON」这条链路上
// 任何一环做有损转换。
func DecodeTransfer(l types.Log) (Transfer, bool) {
	// Transfer 的 from / to 是 indexed 参数，落在 topics[1]、topics[2]；
	// 金额不是 indexed，落在 data 里，固定 32 字节。
	if len(l.Topics) != 3 || l.Topics[0] != TransferTopic {
		return Transfer{}, false
	}
	if len(l.Data) != 32 {
		return Transfer{}, false
	}

	return Transfer{
		Token:       strings.ToLower(l.Address.Hex()),
		BlockNumber: l.BlockNumber,
		TxHash:      l.TxHash.Hex(),
		LogIndex:    l.Index,
		From:        strings.ToLower(common.BytesToAddress(l.Topics[1].Bytes()).Hex()),
		To:          strings.ToLower(common.BytesToAddress(l.Topics[2].Bytes()).Hex()),
		Value:       new(big.Int).SetBytes(l.Data).String(),
	}, true
}
