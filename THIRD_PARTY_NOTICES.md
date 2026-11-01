# Third-party notices

The `lt` binary embeds:

- **uPlot** 1.6.31 (MIT) — `internal/web/assets/uPlot.*`, license in `internal/web/assets/uPlot.LICENSE`.

Go module dependencies and their licenses are listed in `go.mod`
(goja, gorilla/websocket, grpc-go, protobuf-go, protocompile, hdrhistogram-go, yaml.v3).

Database monitoring (`internal/monitor`) links:

| Module | License | Used for |
|---|---|---|
| github.com/jackc/pgx/v5 (+ jackc/pgpassfile, jackc/pgservicefile, jackc/puddle/v2) | MIT | PostgreSQL connections |
| github.com/go-sql-driver/mysql | MPL-2.0 | MySQL connections. Used unmodified; its source is available at https://github.com/go-sql-driver/mysql |
| filippo.io/edwards25519 (dependency of go-sql-driver/mysql) | BSD-3-Clause | MySQL ed25519 authentication |

Each module's full license text ships with its source in the Go module cache
(`go mod download -json <module>` shows where).
