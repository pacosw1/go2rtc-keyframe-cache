package streams

import (
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/rs/zerolog"
)

// KeyframeCacheEnabled controls whether keyframe caching is enabled globally
var KeyframeCacheEnabled = true

// KeyframeCacheMaxPackets is the maximum number of packets to cache per stream
var KeyframeCacheMaxPackets = 500

// KeyframeCacheMaxBytes is the maximum size of the cache in bytes per stream
var KeyframeCacheMaxBytes = 2 * 1024 * 1024 // 2MB

func Init() {
	var cfg struct {
		Streams map[string]any    `yaml:"streams"`
		Publish map[string]any    `yaml:"publish"`
		Preload map[string]string `yaml:"preload"`
		// Keyframe caching configuration for instant playback
		KeyframeCache struct {
			Enabled    *bool `yaml:"enabled"`
			MaxPackets *int  `yaml:"max_packets"`
			MaxBytes   *int  `yaml:"max_bytes"`
		} `yaml:"keyframe_cache"`
	}

	app.LoadConfig(&cfg)

	log = app.GetLogger("streams")

	// Apply keyframe cache configuration
	if cfg.KeyframeCache.Enabled != nil {
		KeyframeCacheEnabled = *cfg.KeyframeCache.Enabled
	}
	if cfg.KeyframeCache.MaxPackets != nil {
		KeyframeCacheMaxPackets = *cfg.KeyframeCache.MaxPackets
	}
	if cfg.KeyframeCache.MaxBytes != nil {
		KeyframeCacheMaxBytes = *cfg.KeyframeCache.MaxBytes
	}

	// Apply configuration to core package
	core.SetGlobalKeyframeCacheConfig(KeyframeCacheEnabled, KeyframeCacheMaxPackets, KeyframeCacheMaxBytes)

	if KeyframeCacheEnabled {
		log.Info().
			Int("max_packets", KeyframeCacheMaxPackets).
			Int("max_bytes", KeyframeCacheMaxBytes).
			Msg("[keyframe-cache] Enabled for instant playback")
	} else {
		log.Info().Msg("[keyframe-cache] Disabled")
	}

	for name, item := range cfg.Streams {
		streams[name] = NewStream(item)
	}

	api.HandleFunc("api/streams", apiStreams)
	api.HandleFunc("api/streams.dot", apiStreamsDOT)
	api.HandleFunc("api/preload", apiPreload)
	api.HandleFunc("api/schemes", apiSchemes)
	api.HandleFunc("api/cache", apiCacheStats)

	if cfg.Publish == nil && cfg.Preload == nil {
		return
	}

	time.AfterFunc(time.Second, func() {
		// range for nil map is OK
		for name, dst := range cfg.Publish {
			if stream := Get(name); stream != nil {
				Publish(stream, dst)
			}
		}
		for name, rawQuery := range cfg.Preload {
			if err := AddPreload(name, rawQuery); err != nil {
				log.Error().Err(err).Caller().Send()
			}
		}
	})
}

func New(name string, sources ...string) (*Stream, error) {
	for _, source := range sources {
		if !HasProducer(source) {
			return nil, errors.New("streams: source not supported")
		}

		if err := Validate(source); err != nil {
			return nil, err
		}
	}

	stream := NewStream(sources)

	streamsMu.Lock()
	streams[name] = stream
	streamsMu.Unlock()

	return stream, nil
}

func Patch(name string, source string) (*Stream, error) {
	streamsMu.Lock()
	defer streamsMu.Unlock()

	// check if source links to some stream name from go2rtc
	if u, err := url.Parse(source); err == nil && u.Scheme == "rtsp" && len(u.Path) > 1 {
		rtspName := u.Path[1:]
		if stream, ok := streams[rtspName]; ok {
			if streams[name] != stream {
				// link (alias) streams[name] to streams[rtspName]
				streams[name] = stream
			}
			return stream, nil
		}
	}

	if stream, ok := streams[source]; ok {
		if name != source {
			// link (alias) streams[name] to streams[source]
			streams[name] = stream
		}
		return stream, nil
	}

	// check if src has supported scheme
	if !HasProducer(source) {
		return nil, errors.New("streams: source not supported")
	}

	if err := Validate(source); err != nil {
		return nil, err
	}

	// check an existing stream with this name
	if stream, ok := streams[name]; ok {
		stream.SetSource(source)
		return stream, nil
	}

	// create new stream with this name
	stream := NewStream(source)
	streams[name] = stream
	return stream, nil
}

func GetOrPatch(query url.Values) (*Stream, error) {
	// check if src param exists
	source := query.Get("src")
	if source == "" {
		return nil, errors.New("streams: source empty")
	}

	// check if src is stream name
	if stream := Get(source); stream != nil {
		return stream, nil
	}

	// check if name param provided
	if name := query.Get("name"); name != "" {
		return Patch(name, source)
	}

	// return new stream with src as name
	return Patch(source, source)
}

var log zerolog.Logger

// streams map

var streams = map[string]*Stream{}
var streamsMu sync.Mutex

func Get(name string) *Stream {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	return streams[name]
}

func Delete(name string) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	delete(streams, name)
}

func GetAllNames() []string {
	streamsMu.Lock()
	names := make([]string, 0, len(streams))
	for name := range streams {
		names = append(names, name)
	}
	streamsMu.Unlock()
	return names
}

func GetAllSources() map[string][]string {
	streamsMu.Lock()
	sources := make(map[string][]string, len(streams))
	for name, stream := range streams {
		sources[name] = stream.Sources()
	}
	streamsMu.Unlock()
	return sources
}
