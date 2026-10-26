// Package store 把撮合引擎的事件日志持久化到 PostgreSQL。
//
// 设计要点：事件日志是唯一的事实来源，成交记录不单独建表。
// 撮合是确定性的——同样的事件按同样的顺序输入，必然产生同样的成交。
// 因此进程重启时只需按 ID 顺序回放全部事件，就能把订单簿和成交历史
// 恢复到崩溃前的状态。这比「分别存订单和成交」少一张表，也不存在
// 两张表写一半导致的互相矛盾。
//
// 下单和撤单必须写进同一张表、共用同一个自增序列。分开两张表的话，
// 「先撤单、后下单」的真实顺序无法还原，回放结果就会与崩溃前不同。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/liliang198909/matching-engine/chain"
)

// schema 是建表语句，使用 IF NOT EXISTS 以便每次启动都能安全执行。
//
// 价格和数量用 DOUBLE PRECISION：引擎内部就是 float64，
// 用 NUMERIC 只会制造「看起来很精确」的假象，转换还要多一层。
// 真正做交易所应当用定点数或 DECIMAL，这属于已知取舍，不是疏漏。
//
// 旧版本只有 orders 表（仅存下单）。下面的 DO 块把它的内容原样搬进
// events 表并保留 ID，使升级前的历史订单仍能正确回放。
const schema = `
DO $$
BEGIN
    IF to_regclass('public.events') IS NULL
       AND to_regclass('public.orders') IS NOT NULL THEN
        CREATE TABLE events (
            id         BIGSERIAL PRIMARY KEY,
            kind       TEXT             NOT NULL CHECK (kind IN ('order', 'cancel')),
            side       TEXT             CHECK (side IN ('buy', 'sell')),
            price      DOUBLE PRECISION CHECK (price > 0),
            size       DOUBLE PRECISION CHECK (size > 0),
            cancel_id  BIGINT,
            created_at TIMESTAMPTZ      NOT NULL DEFAULT now()
        );
        INSERT INTO events (id, kind, side, price, size, created_at)
            SELECT id, 'order', side, price, size, created_at FROM orders ORDER BY id;
        PERFORM setval(
            pg_get_serial_sequence('events', 'id'),
            COALESCE((SELECT max(id) FROM events), 1),
            (SELECT count(*) > 0 FROM events)
        );
        DROP TABLE orders;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS events (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT             NOT NULL CHECK (kind IN ('order', 'cancel')),
    side       TEXT             CHECK (side IN ('buy', 'sell')),
    price      DOUBLE PRECISION CHECK (price > 0),
    size       DOUBLE PRECISION CHECK (size > 0),
    cancel_id  BIGINT,
    created_at TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS events_created_at_idx ON events (created_at);
`

// chainSchema 是链上数据的表结构，与撮合事件完全分开。
//
// 为什么不复用 events 表：events 是撮合引擎的事实来源，回放时必须
// 逐条按顺序重建盘口。链上转账属于另一条写入路径，混进去会让回放
// 读到无法解释的记录。两者唯一共享的只有「同一个数据库」。
//
// value 用 TEXT 而不是 NUMERIC：链上金额是 256 位整数，TEXT 保真无损，
// 也避免了 numeric → big.Int 的额外编解码。代价是无法直接用 SQL 聚合，
// 需要求和时在应用层用 big.Int 算——对演示规模完全够用。
//
// (tx_hash, log_index) 唯一：同一条日志被重复处理时靠 ON CONFLICT DO NOTHING
// 幂等吞掉。进程在「写库」和「推进游标」之间崩溃就会重放同一区间，
// 有这道约束才不会出现重复记录。
const chainSchema = `
CREATE TABLE IF NOT EXISTS chain_transfers (
    id           BIGSERIAL PRIMARY KEY,
    token        TEXT        NOT NULL,
    block_number BIGINT      NOT NULL CHECK (block_number > 0),
    tx_hash      TEXT        NOT NULL,
    log_index    INT         NOT NULL,
    from_addr    TEXT        NOT NULL,
    to_addr      TEXT        NOT NULL,
    value        TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tx_hash, log_index)
);

CREATE INDEX IF NOT EXISTS chain_transfers_block_idx ON chain_transfers (block_number);

CREATE TABLE IF NOT EXISTS chain_cursor (
    id         SMALLINT PRIMARY KEY CHECK (id = 1),
    last_block BIGINT   NOT NULL
);
`

// EventRecord 是从数据库读回的一条事件日志。
type EventRecord struct {
	ID      uint64  // 事件自身的序号
	Kind    string  // "order" 或 "cancel"
	OrderID uint64  // 下单事件为新订单 ID；撤单事件为被撤订单 ID
	Side    string  // 仅下单事件有值
	Price   float64 // 仅下单事件有值
	Size    float64 // 仅下单事件有值
}

// Postgres 持有连接池，并发请求可安全共享。
type Postgres struct {
	pool *pgxpool.Pool
}

// Open 建立连接池并做一次探活。
//
// 连接池而不是单个连接：HTTP 服务同时处理多个请求，
// 每个请求各取一条连接，用完归还，避免自己排队抢同一根连接。
func Open(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &Postgres{pool: pool}, nil
}

// Migrate 创建表结构。建表语句可重复执行，无需另配迁移工具。
func (p *Postgres) Migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	if _, err := p.pool.Exec(ctx, chainSchema); err != nil {
		return fmt.Errorf("migrate chain schema: %w", err)
	}
	return nil
}

// InsertOrder 追加一条下单事件，返回数据库分配的 ID。
//
// ID 由数据库序列生成，意味着订单编号在进程重启后仍然连续，
// 不会像内存计数器那样从 1 重新开始、和历史订单撞号。
func (p *Postgres) InsertOrder(ctx context.Context, side string, price, size float64) (uint64, error) {
	var id uint64
	err := p.pool.QueryRow(ctx,
		`INSERT INTO events (kind, side, price, size) VALUES ('order', $1, $2, $3) RETURNING id`,
		side, price, size,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert order event: %w", err)
	}
	return id, nil
}

// InsertCancel 追加一条撤单事件。
func (p *Postgres) InsertCancel(ctx context.Context, orderID uint64) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO events (kind, cancel_id) VALUES ('cancel', $1)`,
		orderID,
	)
	if err != nil {
		return fmt.Errorf("insert cancel event: %w", err)
	}
	return nil
}

// LoadEvents 按发生顺序读取全部事件，供启动时回放。
func (p *Postgres) LoadEvents(ctx context.Context) ([]EventRecord, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id,
		       kind,
		       COALESCE(cancel_id, id) AS order_id,
		       COALESCE(side, ''),
		       COALESCE(price, 0),
		       COALESCE(size, 0)
		FROM events
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []EventRecord
	for rows.Next() {
		var r EventRecord
		if err := rows.Scan(&r.ID, &r.Kind, &r.OrderID, &r.Side, &r.Price, &r.Size); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return out, nil
}

// LastChainBlock 读取链上数据处理到的高度。ok 为 false 表示从未处理过。
func (p *Postgres) LastChainBlock(ctx context.Context) (uint64, bool, error) {
	var last int64
	err := p.pool.QueryRow(ctx, `SELECT last_block FROM chain_cursor WHERE id = 1`).Scan(&last)
	if err != nil {
		// 游标行不存在是「还没开始处理」，不是故障，用 ok=false 表达。
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read chain cursor: %w", err)
	}
	return uint64(last), true, nil
}

// SaveTransfers 在一个事务里写入一批转账并推进游标。
//
// 两件事必须原子提交：如果先插入记录、后更新游标时崩溃，重启后会重扫
// 同一区间（靠唯一约束幂等吞掉，仅浪费一次拉取）；反过来先更新游标、
// 后插入记录时崩溃，那批转账就永久丢了——这才是真正不可接受的。
// 放进同一事务，两种裂缝都不会出现。
func (p *Postgres) SaveTransfers(ctx context.Context, transfers []chain.Transfer, lastBlock uint64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin chain tx: %w", err)
	}
	// 提交成功后再 Rollback 会返回 ErrTxClosed，属于正常路径，忽略即可。
	defer func() { _ = tx.Rollback(ctx) }()

	for _, t := range transfers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO chain_transfers (token, block_number, tx_hash, log_index, from_addr, to_addr, value)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tx_hash, log_index) DO NOTHING`,
			t.Token, t.BlockNumber, t.TxHash, t.LogIndex, t.From, t.To, t.Value,
		); err != nil {
			return fmt.Errorf("insert transfer %s#%d: %w", t.TxHash, t.LogIndex, err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO chain_cursor (id, last_block) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET last_block = EXCLUDED.last_block`,
		lastBlock,
	); err != nil {
		return fmt.Errorf("advance chain cursor: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chain tx: %w", err)
	}
	return nil
}

// TransferPage 分页读取链上转账，按区块高度与日志序号升序。
//
// 返回当前页与总条数，让调用方能算出总页数，不必自己猜边界。
func (p *Postgres) TransferPage(ctx context.Context, offset, limit int) ([]chain.Transfer, int, error) {
	var total int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM chain_transfers`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count chain transfers: %w", err)
	}

	rows, err := p.pool.Query(ctx, `
		SELECT token, block_number, tx_hash, log_index, from_addr, to_addr, value
		FROM chain_transfers
		ORDER BY block_number, log_index
		LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("query chain transfers: %w", err)
	}
	defer rows.Close()

	out := make([]chain.Transfer, 0, limit)
	for rows.Next() {
		var (
			t   chain.Transfer
			blk int64
			idx int32
		)
		if err := rows.Scan(&t.Token, &blk, &t.TxHash, &idx, &t.From, &t.To, &t.Value); err != nil {
			return nil, 0, fmt.Errorf("scan chain transfer: %w", err)
		}
		t.BlockNumber = uint64(blk)
		t.LogIndex = uint(idx)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate chain transfers: %w", err)
	}
	return out, total, nil
}

// Ping 供健康检查使用。
func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// Close 关闭连接池，等待已借出的连接归还。
func (p *Postgres) Close() {
	p.pool.Close()
}
