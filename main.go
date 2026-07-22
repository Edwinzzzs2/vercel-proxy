package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/tbxark/vercel-proxy/api"
)

// BuildVersion is set at build time via -ldflags.
var BuildVersion = "dev"

func main() {
	defaultAddr := ":3000"
	// Vercel 运行时通过 PORT 分配监听端口，本地和 Docker 仍默认使用 3000。
	if port := os.Getenv("PORT"); port != "" {
		defaultAddr = ":" + port
	}

	addr := flag.String("addr", defaultAddr, "address to listen on")
	configPath := flag.String("config", "", "path to JSON config file")
	flag.Parse()

	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
		return
	}
	config = applyEnvConfig(config)

	proxy, err := api.NewProxy(config)
	if err != nil {
		log.Fatalf("Failed to create proxy: %v", err)
		return
	}

	err = http.ListenAndServe(*addr, proxy)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
		return
	}
}

func applyEnvConfig(config api.Config) api.Config {
	if authToken, ok := os.LookupEnv("PROXY_AUTH_TOKEN"); ok {
		config.AuthToken = strings.TrimSpace(authToken)
	}
	if whitelist, ok := os.LookupEnv("PROXY_DOMAIN_WHITELIST"); ok {
		config.DomainWhitelist = splitCommaSeparated(whitelist)
	}
	return config
}

func splitCommaSeparated(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if entry := strings.TrimSpace(part); entry != "" {
			result = append(result, entry)
		}
	}
	return result
}

func loadConfig(path string) (api.Config, error) {
	if path == "" {
		return api.Config{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return api.Config{}, err
	}

	var config api.Config
	if err := json.Unmarshal(data, &config); err != nil {
		return api.Config{}, err
	}
	return config, nil
}
