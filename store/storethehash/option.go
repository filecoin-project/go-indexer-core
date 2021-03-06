package storethehash

import (
	"time"

	sthtypes "github.com/ipld/go-storethehash/store/types"
)

// TODO: Benchmark and fine-tune for better performance.
const (
	defaultBurstRate     = 4 * 1024 * 1024
	defaultIndexSizeBits = uint8(24)
	defaultIndexFileSize = uint32(1024 * 1024 * 1024)
	defaultSyncInterval  = time.Second
	defaultGCInterval    = 30 * time.Minute
)

// config contains all options for configuring storethehash valuestore.
type config struct {
	burstRate     sthtypes.Work
	indexSizeBits uint8
	indexFileSize uint32
	syncInterval  time.Duration
	gcInterval    time.Duration
}

type Option func(*config)

// apply applies the given options to this config.
func (c *config) apply(opts []Option) {
	for _, opt := range opts {
		opt(c)
	}
}

func IndexBitSize(indexBitSize uint8) Option {
	return func(cfg *config) {
		cfg.indexSizeBits = indexBitSize
	}
}

func IndexFileSize(indexFileSize uint32) Option {
	return func(cfg *config) {
		cfg.indexFileSize = indexFileSize
	}
}

func SyncInterval(syncInterval time.Duration) Option {
	return func(cfg *config) {
		cfg.syncInterval = syncInterval
	}
}

func BurstRate(burstRate uint64) Option {
	return func(cfg *config) {
		cfg.burstRate = sthtypes.Work(burstRate)
	}
}

func GCInterval(gcInterval time.Duration) Option {
	return func(cfg *config) {
		cfg.gcInterval = gcInterval
	}
}
