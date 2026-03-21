package server

import "time"

// Config holds all server configuration, populated from environment variables.
type Config struct {
	Version              string
	Port                 int           `env:"SYL_PORT"                  envDefault:"8080"`
	InternalPort         int           `env:"SYL_INTERNAL_PORT"         envDefault:"9090"`
	DBPath               string        `env:"SYL_DB_PATH"               envDefault:"data/syl.db"`
	AnthropicAPIKey      string        `env:"SYL_ANTHROPIC_API_KEY"`
	Name                 string        `env:"SYL_NAME"                  envDefault:"Syl"`
	Debug                bool          `env:"SYL_DEBUG"                 envDefault:"false"`
	SkillsDir            string        `env:"SYL_SKILLS_DIR"            envDefault:"skills"`
	RequestTimeout       time.Duration `env:"SYL_REQUEST_TIMEOUT"       envDefault:"30s"`
	AsyncTimeout         time.Duration `env:"SYL_ASYNC_TIMEOUT"         envDefault:"60s"`
	ShutdownTimeout      time.Duration `env:"SYL_SHUTDOWN_TIMEOUT"      envDefault:"30s"`
	CompactionThreshold  int           `env:"SYL_COMPACTION_THRESHOLD"  envDefault:"50"`
	HistoryLimit         int           `env:"SYL_HISTORY_LIMIT"         envDefault:"20"`
	FetchDenylist        []string      `env:"SYL_FETCH_DENYLIST"        envSeparator:","`
	FetchContentLimit    int           `env:"SYL_FETCH_CONTENT_LIMIT"   envDefault:"4000"`
}
