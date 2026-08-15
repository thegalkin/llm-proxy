package proxy

import (
	"os"
	"strings"
)

// --- .env loader (same as minimax-proxy) ---

func LoadDotEnv() {
	path := os.Getenv("ENV_FILE")
	if path == "" {
		path = ".env"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		v = strings.Trim(v, "\"'")
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}
