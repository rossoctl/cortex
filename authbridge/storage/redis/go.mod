module github.com/rossoctl/cortex/authbridge/storage/redis

go 1.26.4

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/redis/go-redis/v9 v9.9.0
	github.com/rossoctl/cortex/authbridge/authlib v0.0.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
)

replace github.com/rossoctl/cortex/authbridge/authlib => ../../authlib
