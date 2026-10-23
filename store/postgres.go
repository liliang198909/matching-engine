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
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// Ping 供健康检查使用。
func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// Close 关闭连接池，等待已借出的连接归还。
func (p *Postgres) Close() {
	p.pool.Close()
}
