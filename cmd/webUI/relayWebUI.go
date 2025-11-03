package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

//go:embed static
var staticFiles embed.FS

type RelayConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	UDP  bool   `json:"udp"`
}

type Storage struct {
	mu      sync.RWMutex
	file    string
	configs []RelayConfig
}

func NewStorage(file string) *Storage {
	s := &Storage{
		file:    file,
		configs: []RelayConfig{},
	}
	s.load()
	return s
}

func (s *Storage) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.file)
	if err != nil {
		if os.IsNotExist(err) {
			// Create empty file with sample data
			s.configs = []RelayConfig{
				{
					ID:   uuid.New().String(),
					Name: "DNS Relay",
					Src:  ":53",
					Dst:  "1.1.1.1:53",
					UDP:  true,
				},
			}
			return s.save()
		}
		return err
	}

	return json.Unmarshal(data, &s.configs)
}

func (s *Storage) save() error {
	data, err := json.MarshalIndent(s.configs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.file, data, 0644)
}

func (s *Storage) List() []RelayConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]RelayConfig{}, s.configs...)
}

func (s *Storage) Get(id string) *RelayConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, cfg := range s.configs {
		if cfg.ID == id {
			return &cfg
		}
	}
	return nil
}

func (s *Storage) Create(cfg RelayConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.ID = uuid.New().String()
	s.configs = append(s.configs, cfg)
	return s.save()
}

func (s *Storage) Update(id string, cfg RelayConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.configs {
		if c.ID == id {
			cfg.ID = id
			s.configs[i] = cfg
			return s.save()
		}
	}
	return nil
}

func (s *Storage) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.configs {
		if c.ID == id {
			s.configs = append(s.configs[:i], s.configs[i+1:]...)
			return s.save()
		}
	}
	return nil
}

func main() {
	storage := NewStorage("relays.json")

	r := gin.Default()

	// Serve embedded static files
	staticFS, _ := fs.Sub(staticFiles, "static")
	r.StaticFS("/static", http.FS(staticFS))
	r.GET("/", func(c *gin.Context) {
		data, _ := staticFiles.ReadFile("static/index.html")
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})

	// API routes
	api := r.Group("/api")
	{
		// List all relays
		api.GET("/relays", func(c *gin.Context) {
			c.JSON(http.StatusOK, storage.List())
		})

		// Get single relay
		api.GET("/relays/:id", func(c *gin.Context) {
			cfg := storage.Get(c.Param("id"))
			if cfg == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			c.JSON(http.StatusOK, cfg)
		})

		// Create relay
		api.POST("/relays", func(c *gin.Context) {
			var cfg RelayConfig
			if err := c.BindJSON(&cfg); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if err := storage.Create(cfg); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusCreated, cfg)
		})

		// Update relay
		api.PUT("/relays/:id", func(c *gin.Context) {
			var cfg RelayConfig
			if err := c.BindJSON(&cfg); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if err := storage.Update(c.Param("id"), cfg); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, cfg)
		})

		// Delete relay
		api.DELETE("/relays/:id", func(c *gin.Context) {
			if err := storage.Delete(c.Param("id")); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"message": "deleted"})
		})
	}

	log.Println("WebUI server starting on :8080")
	log.Println("Open http://localhost:8080 in your browser")
	r.Run(":8080")
}
