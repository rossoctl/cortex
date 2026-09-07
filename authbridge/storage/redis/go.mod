module github.com/rossoctl/cortex/authbridge/storage/redis

go 1.26.5

require (
	github.com/alicebob/miniredis/v2 v2.39.0
	github.com/redis/go-redis/v9 v9.22.0
	github.com/rossoctl/cortex/authbridge/authlib v0.0.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/rossoctl/cortex/authbridge/authlib => ../../authlib
