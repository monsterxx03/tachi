package logger

// Config holds logger configuration, typically loaded from config.yaml under "logs:".
type Config struct {
	// Level is the minimum log level. Default: "info".
	Level string `yaml:"level" default:"info"`

	// MaxSize is the max size per log file before rotation. Default: "10mb".
	MaxSize string `yaml:"max_size" default:"10mb"`

	// MaxFiles is the number of rotated files to keep. Default: 10.
	MaxFiles int `yaml:"max_files" default:"10"`

	// PerEntry controls whether logs are split by entry point into separate files.
	// Default: true.
	PerEntry bool `yaml:"per_entry" default:"true"`
}

// DefaultConfig returns a Config with default values applied.
func DefaultConfig() *Config {
	return &Config{
		Level:    "info",
		MaxSize:  "10mb",
		MaxFiles: 10,
		PerEntry: true,
	}
}
