module github.com/faultvane/faultvane-go/scenarios

go 1.26.0

require (
	github.com/faultvane/faultvane-go v0.3.0
	github.com/faultvane/faultvane-go/store/mongodb v0.3.0
	github.com/faultvane/faultvane-go/store/redis v0.3.0
	github.com/faultvane/faultvane-go/store/sql v0.3.0
	github.com/go-sql-driver/mysql v1.10.2
	github.com/jackc/pgx/v5 v5.13.0
	github.com/redis/go-redis/v9 v9.23.0
	go.mongodb.org/mongo-driver/v2 v2.10.1
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/xdg-go/scram v1.4.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

replace (
	github.com/faultvane/faultvane-go => ..
	github.com/faultvane/faultvane-go/store/mongodb => ../store/mongodb
	github.com/faultvane/faultvane-go/store/redis => ../store/redis
	github.com/faultvane/faultvane-go/store/sql => ../store/sql
)
