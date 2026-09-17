package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"

	"timingstation/api/internal/app"
	"timingstation/api/internal/store"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	port := envOr("PORT", "8080")
	dbPath := envOr("DB_PATH", "data/app.db")

	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("create db dir: %v", err)
		}
	}
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	r := app.NewRouter(st)
	log.Printf("timing-station api listening on :%s (db=%s)", port, dbPath)
	if err := r.Run(":" + port); err != nil {
		log.Fatal(err)
	}
}
