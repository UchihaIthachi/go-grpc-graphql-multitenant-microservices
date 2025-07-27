CREATE KEYSPACE IF NOT EXISTS orders WITH REPLICATION = { 'class' : 'SimpleStrategy', 'replication_factor' : 1 };

CREATE TYPE IF NOT EXISTS orders.product (
    id text,
    quantity int,
    price float
);

CREATE TABLE IF NOT EXISTS orders.orders (
    id text,
    created_at timestamp,
    account_id text,
    total_price float,
    products list<frozen<product>>,
    PRIMARY KEY (account_id, created_at, id)
) WITH CLUSTERING ORDER BY (created_at DESC);