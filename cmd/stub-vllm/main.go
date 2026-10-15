package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelsResponse struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	port := flag.String("port", "8000", "listen port")
	host := flag.String("host", "0.0.0.0", "listen host")
	modelName := flag.String("model", "Qwen/Qwen2.5-32B-Instruct", "model name")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"model":  *modelName,
			"time":   time.Now().Unix(),
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, modelsResponse{
			Object: "list",
			Data:   []modelItem{{ID: *modelName, Object: "model", OwnedBy: "stub"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		var req chatRequest
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		prompt := ""
		for _, m := range req.Messages {
			prompt += m.Content + "\n"
		}
		d := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(strings.TrimSpace(prompt))))
		resp := chatResponse{
			ID:      fmt.Sprintf("chatcmpl-stub-%d", time.Now().UnixNano()),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []chatChoice{{
				Index:        0,
				Message:      chatMessage{Role: "assistant", Content: fmt.Sprintf("[vllm-stub] model=%s prompt_sha256=%s", *modelName, d)},
				FinishReason: "stop",
			}},
		}
		writeJSON(w, http.StatusOK, resp)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", *host, *port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "stub-vllm listening on %s:%s model=%s\n", *host, *port, *modelName)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}
