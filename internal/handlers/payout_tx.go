package handlers

import "github.com/jackc/pgx/v5"

func pgxTxOptions() pgx.TxOptions { return pgx.TxOptions{} }
