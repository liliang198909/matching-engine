// Package store 把撮合引擎的订单日志持久化到 PostgreSQL。
//
// 设计要点：订单表是唯一的事实来源，成交记录不单独建表。
// 撮合是确定性的——同样的订单按同样的顺序输入，必然产生同样的成交。
// 因此进程重启时只需按 ID 顺序回放全部订单，就能把订单簿和成交历史
// 恢复到崩溃前的状态。这比「分别存订单和成交」少一张表，也不存在
// 两张表写一半导致的互相矛盾。
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schema 是建表语句，使用 IF NOT EXISTS 以便每次启动都能安全执行。
//
// 价格和数量用 DOUBLE PRECISION：引擎内部就是 float64，
// 用 NUMERIC 只会制造「看起来很精确」的假象，转换还要多一层。
// 真正做交易所应当用定点数或 DECIMAL，这属于已知取舍，不是疏漏。
const schema = `
CREATE TABLE IF NOT EXISTS orders (
    id         BIGSERIAL PRIMARY KEY,
    side       TEXT             NOT NULL CHECK (side IN ('buy', 'sell')),
    price      DOUBLE PRECISION NOT NULL CHECK (price > 0),
    size       DOUBLE PRECISION NOT NULL CHECK (size > 0),
    created_at TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS orders_created_at_idx ON orders (created_at);
`

// OrderRecord 是从数据库读回的一条订单日志。
type OrderRecord struct {
	ID    uint64
	Side  string
	Price float64
	Size  float64
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
	return nil
}

// InsertOrder 追加一条订单日志，返回数据库分配的 ID。
//
// ID 由数据库序列生成，意味着订单编号在进程重启后仍然连续，
// 不会像内存计数器那样从 1 重新开始、和历史订单撞号。
func (p *Postgres) InsertOrder(ctx context.Context, side string, price, size float64) (uint64, error) {
	var id uint64
	err := p.pool.QueryRow(ctx,
		`INSERT INTO orders (side, price, size) VALUES ($1, $2, $3) RETURNING id`,
		side, price, size,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert order: %w", err)
	}
	return id, nil
}

// LoadOrders 按 ID 顺序读取全部订单，供启动时回放。
func (p *Postgres) LoadOrders(ctx context.Context) ([]OrderRecord, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, side, price, size FROM orders ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query orders: %w", err)
	}
	defer rows.Close()

	var out []OrderRecord
	for rows.Next() {
		var r OrderRecord
		if err := rows.Scan(&r.ID, &r.Side, &r.Price, &r.Size); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orders: %w", err)
	}
	return out, nil
}

// Ping 供健康检查使用。
func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// Close 关闭连接池，等待已借出的连接归还。
func (p *Postgres) Close() {
	p.pool.Close()
}
